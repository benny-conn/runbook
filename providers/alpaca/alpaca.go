// Package alpaca implements provider.MarketData and provider.Execution using the
// Alpaca Markets API. Reads ALPACA_API_KEY, ALPACA_SECRET, and ALPACA_BASE_URL from env.
package alpaca

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	alp "github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata/stream"
	"github.com/shopspring/decimal"

	"github.com/benny-conn/brandon-bot/provider"
	"github.com/benny-conn/brandon-bot/strategy"
)

// Config holds Alpaca provider credentials and settings.
type Config struct {
	APIKey  string `json:"api_key"`
	Secret  string `json:"secret"`
	BaseURL string `json:"base_url"`
	Feed    string `json:"feed"`
}

// Provider implements provider.MarketData and provider.Execution.
type Provider struct {
	trading *alp.Client
	md      *marketdata.Client
	feed    marketdata.Feed
	apiKey  string
	secret  string

	// Pending stream handlers registered before the single shared connection is opened.
	streamMu     sync.Mutex
	tradeHandler func(stream.Trade)
	tradeSymbols []string
	quoteHandler func(stream.Quote)
	quoteSymbols []string
}

// New creates an Alpaca provider. Config fields override env vars where set.
func New(cfg Config) *Provider {
	apiKey := envOr(cfg.APIKey, "ALPACA_API_KEY")
	secret := envOr(cfg.Secret, "ALPACA_SECRET")
	baseURL := envOr(cfg.BaseURL, "ALPACA_BASE_URL")
	feed := envOr(cfg.Feed, "")
	if feed == "" {
		feed = "iex"
	}
	return &Provider{
		trading: alp.NewClient(alp.ClientOpts{
			APIKey:    apiKey,
			APISecret: secret,
			BaseURL:   baseURL,
		}),
		md: marketdata.NewClient(marketdata.ClientOpts{
			APIKey:    apiKey,
			APISecret: secret,
		}),
		feed:   marketdata.Feed(feed),
		apiKey: apiKey,
		secret: secret,
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
	tf, err := parseTimeFrame(timeframe)
	if err != nil {
		return nil, err
	}
	bars, err := p.md.GetBars(symbol, marketdata.GetBarsRequest{
		TimeFrame: tf,
		Start:     start,
		End:       end,
		Feed:      p.feed,
	})
	if err != nil {
		return nil, fmt.Errorf("fetching bars for %s: %w", symbol, err)
	}
	result := make([]provider.Bar, len(bars))
	for i, b := range bars {
		result[i] = provider.Bar{
			Symbol:    symbol,
			Timestamp: b.Timestamp,
			Open:      b.Open,
			High:      b.High,
			Low:       b.Low,
			Close:     b.Close,
			Volume:    float64(b.Volume),
		}
	}
	return result, nil
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
	opts := []stream.StockOption{
		stream.WithCredentials(p.apiKey, p.secret),
		stream.WithBars(func(b stream.Bar) {
			handler(provider.Bar{
				Symbol:    b.Symbol,
				Timestamp: b.Timestamp,
				Open:      b.Open,
				High:      b.High,
				Low:       b.Low,
				Close:     b.Close,
				Volume:    float64(b.Volume),
			})
		}, symbols...),
	}

	// Include any trade/quote handlers registered before this call so
	// everything runs over a single WebSocket connection.
	p.streamMu.Lock()
	if p.tradeHandler != nil {
		opts = append(opts, stream.WithTrades(p.tradeHandler, p.tradeSymbols...))
	}
	if p.quoteHandler != nil {
		opts = append(opts, stream.WithQuotes(p.quoteHandler, p.quoteSymbols...))
	}
	p.streamMu.Unlock()

	sc := stream.NewStocksClient(p.feed, opts...)
	if err := sc.Connect(ctx); err != nil {
		return fmt.Errorf("alpaca bar stream connect: %w", err)
	}
	<-ctx.Done()
	return nil
}

func (p *Provider) SubscribeTrades(ctx context.Context, symbols []string, handler func(provider.Trade)) error {
	// Register the trade handler to be included in the shared stream connection
	// created by SubscribeBars. This avoids opening multiple WebSocket connections
	// which would exceed Alpaca's per-key connection limit.
	p.streamMu.Lock()
	p.tradeHandler = func(t stream.Trade) {
		handler(provider.Trade{
			Symbol:    t.Symbol,
			Timestamp: t.Timestamp,
			Price:     t.Price,
			Size:      float64(t.Size),
		})
	}
	p.tradeSymbols = symbols
	p.streamMu.Unlock()
	<-ctx.Done()
	return nil
}

// — Execution —

func (p *Provider) GetAccount(ctx context.Context) (provider.Account, error) {
	acct, err := p.trading.GetAccount()
	if err != nil {
		return provider.Account{}, fmt.Errorf("alpaca get account: %w", err)
	}
	return provider.Account{
		Cash:   acct.Cash.InexactFloat64(),
		Equity: acct.Equity.InexactFloat64(),
	}, nil
}

func (p *Provider) GetPositions(ctx context.Context) ([]provider.Position, error) {
	positions, err := p.trading.GetPositions()
	if err != nil {
		return nil, fmt.Errorf("alpaca get positions: %w", err)
	}
	result := make([]provider.Position, len(positions))
	for i, pos := range positions {
		var currentPrice float64
		if pos.CurrentPrice != nil {
			currentPrice = pos.CurrentPrice.InexactFloat64()
		}
		result[i] = provider.Position{
			Symbol:        pos.Symbol,
			Qty:           pos.Qty.InexactFloat64(),
			AvgEntryPrice: pos.AvgEntryPrice.InexactFloat64(),
			CurrentPrice:  currentPrice,
		}
	}
	return result, nil
}

func (p *Provider) PlaceOrder(ctx context.Context, order strategy.Order) (provider.OrderResult, error) {
	qty := decimal.NewFromFloat(order.Qty)
	side := alp.Buy
	if order.Side == "sell" {
		side = alp.Sell
	}
	req := alp.PlaceOrderRequest{
		Symbol:      order.Symbol,
		Qty:         &qty,
		Side:        side,
		Type:        alp.Market,
		TimeInForce: alp.Day,
	}
	if order.OrderType == "limit" && order.LimitPrice > 0 {
		lp := decimal.NewFromFloat(order.LimitPrice)
		req.Type = alp.Limit
		req.LimitPrice = &lp
		req.TimeInForce = alp.GTC
	}
	placed, err := p.trading.PlaceOrder(req)
	if err != nil {
		return provider.OrderResult{}, fmt.Errorf("placing %s order for %s qty=%.2f: %w",
			order.Side, order.Symbol, order.Qty, err)
	}
	return provider.OrderResult{ID: placed.ID}, nil
}

func (p *Provider) SubscribeFills(ctx context.Context, handler func(provider.Fill)) error {
	p.trading.StreamTradeUpdatesInBackground(ctx, func(update alp.TradeUpdate) {
		if update.Event != "fill" && update.Event != "partial_fill" {
			return
		}
		if update.Price == nil || update.Qty == nil {
			return
		}
		handler(provider.Fill{
			OrderID:   update.Order.ID,
			Symbol:    update.Order.Symbol,
			Side:      string(update.Order.Side),
			Qty:       update.Qty.InexactFloat64(),
			Price:     update.Price.InexactFloat64(),
			Timestamp: update.At,
			Partial:   update.Event == "partial_fill",
		})
	})
	<-ctx.Done()
	return nil
}

func (p *Provider) SubscribeQuotes(ctx context.Context, symbols []string, handler func(provider.Quote)) error {
	// Register the quote handler to be included in the shared stream connection
	// created by SubscribeBars.
	p.streamMu.Lock()
	p.quoteHandler = func(q stream.Quote) {
		handler(provider.Quote{
			Symbol:    q.Symbol,
			Timestamp: q.Timestamp,
			BidPrice:  q.BidPrice,
			BidSize:   float64(q.BidSize),
			AskPrice:  q.AskPrice,
			AskSize:   float64(q.AskSize),
		})
	}
	p.quoteSymbols = symbols
	p.streamMu.Unlock()
	<-ctx.Done()
	return nil
}

func (p *Provider) CancelOrder(ctx context.Context, orderID string) error {
	if err := p.trading.CancelOrder(orderID); err != nil {
		return fmt.Errorf("alpaca cancel order %s: %w", orderID, err)
	}
	return nil
}

func (p *Provider) GetOpenOrders(ctx context.Context) ([]provider.OpenOrder, error) {
	orders, err := p.trading.GetOrders(alp.GetOrdersRequest{Status: "open"})
	if err != nil {
		return nil, fmt.Errorf("alpaca get open orders: %w", err)
	}
	result := make([]provider.OpenOrder, len(orders))
	for i, o := range orders {
		var qty, limitPrice, stopPrice float64
		if o.Qty != nil {
			qty = o.Qty.InexactFloat64()
		}
		if o.LimitPrice != nil {
			limitPrice = o.LimitPrice.InexactFloat64()
		}
		if o.StopPrice != nil {
			stopPrice = o.StopPrice.InexactFloat64()
		}
		result[i] = provider.OpenOrder{
			ID:         o.ID,
			Symbol:     o.Symbol,
			Side:       string(o.Side),
			Qty:        qty,
			Filled:     o.FilledQty.InexactFloat64(),
			OrderType:  string(o.Type),
			LimitPrice: limitPrice,
			StopPrice:  stopPrice,
		}
	}
	return result, nil
}

// — AssetSearch —

// SearchAssets implements strategy.AssetSearch using Alpaca's asset listing API.
// Note: Alpaca doesn't support server-side text search, so text filtering is
// done client-side after fetching all matching assets.
func (p *Provider) SearchAssets(ctx context.Context, query strategy.AssetQuery) ([]strategy.Asset, error) {
	req := alp.GetAssetsRequest{
		Status:     query.Status,
		AssetClass: query.AssetClass,
		Exchange:   query.Exchange,
	}
	if req.Status == "" {
		req.Status = "active"
	}

	assets, err := p.trading.GetAssets(req)
	if err != nil {
		return nil, fmt.Errorf("alpaca search assets: %w", err)
	}

	textLower := strings.ToLower(query.Text)
	result := make([]strategy.Asset, 0)
	for _, a := range assets {
		if textLower != "" && !strings.Contains(strings.ToLower(a.Symbol), textLower) &&
			!strings.Contains(strings.ToLower(a.Name), textLower) {
			continue
		}
		result = append(result, strategy.Asset{
			Symbol:     a.Symbol,
			Name:       a.Name,
			AssetClass: string(a.Class),
			Exchange:   a.Exchange,
			Tradable:   a.Tradable,
			Extra: map[string]any{
				"fractionable": a.Fractionable,
				"shortable":    a.Shortable,
				"marginable":   a.Marginable,
			},
		})
		if query.Limit > 0 && len(result) >= query.Limit {
			break
		}
	}
	return result, nil
}

func parseTimeFrame(tf string) (marketdata.TimeFrame, error) {
	switch tf {
	case "1m":
		return marketdata.NewTimeFrame(1, marketdata.Min), nil
	case "5m":
		return marketdata.NewTimeFrame(5, marketdata.Min), nil
	case "15m":
		return marketdata.NewTimeFrame(15, marketdata.Min), nil
	case "1h":
		return marketdata.NewTimeFrame(1, marketdata.Hour), nil
	case "1d":
		return marketdata.NewTimeFrame(1, marketdata.Day), nil
	default:
		return marketdata.TimeFrame{}, fmt.Errorf("unsupported timeframe %q — use 1m, 5m, 15m, 1h, or 1d", tf)
	}
}
