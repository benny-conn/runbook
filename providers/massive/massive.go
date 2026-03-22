// Package massive implements provider.MarketData using the Massive (formerly
// Polygon.io) API. This is a data-only provider — it does not implement
// provider.Execution. Pair it with an execution provider like Alpaca for
// live trading with sub-minute bar data.
//
// Reads MASSIVE_API_KEY from env.
package massive

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"time"

	"github.com/massive-com/client-go/v3/rest"
	"github.com/massive-com/client-go/v3/rest/gen"
	massivews "github.com/massive-com/client-go/v3/websocket"
	"github.com/massive-com/client-go/v3/websocket/models"

	"github.com/benny-conn/brandon-bot/provider"
)

// Config holds Massive provider credentials and settings.
type Config struct {
	APIKey string `json:"api_key"`
	Feed   string `json:"feed"` // "realtime", "delayed", etc. Default: realtime
}

// Provider implements provider.MarketData using Massive.
type Provider struct {
	client *rest.Client
	apiKey string
	feed   massivews.Feed
}

// New creates a Massive market-data provider.
func New(cfg Config) *Provider {
	apiKey := envOr(cfg.APIKey, "MASSIVE_API_KEY")
	feed := massivews.RealTime
	if cfg.Feed == "delayed" {
		feed = massivews.Delayed
	}
	return &Provider{
		client: rest.New(apiKey),
		apiKey: apiKey,
		feed:   feed,
	}
}

func envOr(val, envKey string) string {
	if val != "" {
		return val
	}
	return os.Getenv(envKey)
}

// — MarketData —

func (p *Provider) FetchBars(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]provider.Bar, error) {
	if timeframe == "1s" {
		return p.fetchBarsFromTrades(ctx, symbol, start, end)
	}

	mult, span, err := parseTimeframe(timeframe)
	if err != nil {
		return nil, err
	}

	from := start.Format("2006-01-02")
	to := end.Format("2006-01-02")
	params := &gen.GetStocksAggregatesParams{
		Adjusted: rest.Ptr(true),
		Sort:     "asc",
		Limit:    rest.Int(50000),
	}

	resp, err := p.client.GetStocksAggregatesWithResponse(ctx, symbol, mult, span, from, to, params)
	if err != nil {
		return nil, fmt.Errorf("massive: fetching bars for %s: %w", symbol, err)
	}
	if err := rest.CheckResponse(resp); err != nil {
		return nil, fmt.Errorf("massive: bars response for %s: %w", symbol, err)
	}

	if resp.JSON200.Results == nil {
		return nil, nil
	}

	results := *resp.JSON200.Results
	bars := make([]provider.Bar, 0, len(results))
	for _, r := range results {
		ts := time.Unix(0, int64(r.Timestamp)*int64(time.Millisecond)).UTC()
		if ts.Before(start) || ts.After(end) {
			continue
		}
		bars = append(bars, provider.Bar{
			Symbol:    symbol,
			Timestamp: ts,
			Open:      r.O,
			High:      r.H,
			Low:       r.L,
			Close:     r.C,
			Volume:    r.V,
		})
	}
	return bars, nil
}

// fetchBarsFromTrades builds 1-second OHLCV bars from raw trade data.
func (p *Provider) fetchBarsFromTrades(ctx context.Context, symbol string, start, end time.Time) ([]provider.Bar, error) {
	startNano := fmt.Sprintf("%d", start.UnixNano())
	endNano := fmt.Sprintf("%d", end.UnixNano())
	order := gen.GetStocksTradesParamsOrder("asc")
	sortBy := gen.GetStocksTradesParamsSort("timestamp")

	var bars []provider.Bar
	type bucket struct {
		open, high, low, close, volume float64
		ts                             time.Time
		valid                          bool
	}
	var cur bucket

	flush := func() {
		if cur.valid {
			bars = append(bars, provider.Bar{
				Symbol:    symbol,
				Timestamp: cur.ts,
				Open:      cur.open,
				High:      cur.high,
				Low:       cur.low,
				Close:     cur.close,
				Volume:    cur.volume,
			})
		}
		cur = bucket{}
	}

	cursor := startNano
	for {
		params := &gen.GetStocksTradesParams{
			TimestampGte: &cursor,
			TimestampLt:  &endNano,
			Order:        &order,
			Sort:         &sortBy,
			Limit:        rest.Int(50000),
		}

		resp, err := p.client.GetStocksTradesWithResponse(ctx, symbol, params)
		if err != nil {
			return nil, fmt.Errorf("massive: fetching trades for %s: %w", symbol, err)
		}
		if err := rest.CheckResponse(resp); err != nil {
			return nil, fmt.Errorf("massive: trades response for %s: %w", symbol, err)
		}

		if resp.JSON200.Results == nil || len(*resp.JSON200.Results) == 0 {
			break
		}

		results := *resp.JSON200.Results
		for _, t := range results {
			ts := time.Unix(0, t.SipTimestamp).UTC()
			sec := ts.Truncate(time.Second)

			if cur.valid && sec != cur.ts {
				flush()
			}

			if !cur.valid {
				cur = bucket{
					open: t.Price, high: t.Price, low: t.Price, close: t.Price,
					volume: t.Size, ts: sec, valid: true,
				}
			} else {
				if t.Price > cur.high {
					cur.high = t.Price
				}
				if t.Price < cur.low {
					cur.low = t.Price
				}
				cur.close = t.Price
				cur.volume += t.Size
			}
		}

		// Paginate: move cursor past the last trade.
		last := results[len(results)-1]
		cursor = fmt.Sprintf("%d", last.SipTimestamp+1)

		if len(results) < 50000 {
			break
		}
	}
	flush()
	return bars, nil
}

func (p *Provider) FetchBarsMulti(ctx context.Context, symbols []string, timeframe string, start, end time.Time) ([]provider.Bar, error) {
	type result struct {
		bars []provider.Bar
		err  error
	}
	ch := make(chan result, len(symbols))
	for _, sym := range symbols {
		go func(s string) {
			bars, err := p.FetchBars(ctx, s, timeframe, start, end)
			ch <- result{bars, err}
		}(sym)
	}
	var all []provider.Bar
	for range symbols {
		r := <-ch
		if r.err != nil {
			return nil, r.err
		}
		all = append(all, r.bars...)
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].Timestamp.Before(all[j].Timestamp)
	})
	return all, nil
}

func (p *Provider) SubscribeBars(ctx context.Context, symbols []string, timeframe string, handler func(provider.Bar)) error {
	topic := massivews.StocksMinAggs
	if timeframe == "1s" {
		topic = massivews.StocksSecAggs
	}

	c, err := massivews.New(massivews.Config{
		APIKey: p.apiKey,
		Feed:   p.feed,
		Market: massivews.Stocks,
	})
	if err != nil {
		return fmt.Errorf("massive: bar websocket init: %w", err)
	}

	if err := c.Connect(); err != nil {
		return fmt.Errorf("massive: bar websocket connect: %w", err)
	}
	defer c.Close()

	c.Subscribe(topic, symbols...)

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-c.Error():
			log.Printf("massive: bar stream error: %v", err)
		case out, ok := <-c.Output():
			if !ok {
				return nil
			}
			if agg, ok := out.(models.EquityAgg); ok {
				handler(provider.Bar{
					Symbol:    agg.Symbol,
					Timestamp: time.Unix(0, agg.StartTimestamp*int64(time.Millisecond)).UTC(),
					Open:      agg.Open,
					High:      agg.High,
					Low:       agg.Low,
					Close:     agg.Close,
					Volume:    agg.Volume,
				})
			}
		}
	}
}

func (p *Provider) SubscribeTrades(ctx context.Context, symbols []string, handler func(provider.Trade)) error {
	c, err := massivews.New(massivews.Config{
		APIKey: p.apiKey,
		Feed:   p.feed,
		Market: massivews.Stocks,
	})
	if err != nil {
		return fmt.Errorf("massive: trade websocket init: %w", err)
	}

	if err := c.Connect(); err != nil {
		return fmt.Errorf("massive: trade websocket connect: %w", err)
	}
	defer c.Close()

	c.Subscribe(massivews.StocksTrades, symbols...)

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-c.Error():
			log.Printf("massive: trade stream error: %v", err)
		case out, ok := <-c.Output():
			if !ok {
				return nil
			}
			if t, ok := out.(models.EquityTrade); ok {
				handler(provider.Trade{
					Symbol:    t.Symbol,
					Timestamp: time.Unix(0, t.Timestamp*int64(time.Millisecond)).UTC(),
					Price:     t.Price,
					Size:      float64(t.Size),
				})
			}
		}
	}
}

func (p *Provider) SubscribeQuotes(ctx context.Context, symbols []string, handler func(provider.Quote)) error {
	c, err := massivews.New(massivews.Config{
		APIKey: p.apiKey,
		Feed:   p.feed,
		Market: massivews.Stocks,
	})
	if err != nil {
		return fmt.Errorf("massive: quote websocket init: %w", err)
	}

	if err := c.Connect(); err != nil {
		return fmt.Errorf("massive: quote websocket connect: %w", err)
	}
	defer c.Close()

	c.Subscribe(massivews.StocksQuotes, symbols...)

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-c.Error():
			log.Printf("massive: quote stream error: %v", err)
		case out, ok := <-c.Output():
			if !ok {
				return nil
			}
			if q, ok := out.(models.EquityQuote); ok {
				handler(provider.Quote{
					Symbol:    q.Symbol,
					Timestamp: time.Unix(0, q.Timestamp*int64(time.Millisecond)).UTC(),
					BidPrice:  q.BidPrice,
					BidSize:   float64(q.BidSize),
					AskPrice:  q.AskPrice,
					AskSize:   float64(q.AskSize),
				})
			}
		}
	}
}

// parseTimeframe converts canonical timeframe strings to Massive API parameters.
func parseTimeframe(tf string) (int, gen.GetStocksAggregatesParamsTimespan, error) {
	switch tf {
	case "1m":
		return 1, gen.GetStocksAggregatesParamsTimespan("minute"), nil
	case "5m":
		return 5, gen.GetStocksAggregatesParamsTimespan("minute"), nil
	case "15m":
		return 15, gen.GetStocksAggregatesParamsTimespan("minute"), nil
	case "1h":
		return 1, gen.GetStocksAggregatesParamsTimespan("hour"), nil
	case "1d":
		return 1, gen.GetStocksAggregatesParamsTimespan("day"), nil
	default:
		return 0, "", fmt.Errorf("unsupported timeframe %q — use 1s, 1m, 5m, 15m, 1h, or 1d", tf)
	}
}
