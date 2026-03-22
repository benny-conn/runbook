// Package coinbase implements provider.MarketData and provider.Execution using
// the Coinbase Advanced Trade API. Provides crypto trading with real-time
// WebSocket streaming and REST-based order management.
//
// Reads COINBASE_ACCESS_KEY and COINBASE_PRIVATE_PEM_KEY from env.
package coinbase

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coinbase-samples/advanced-trade-sdk-go/accounts"
	cbclient "github.com/coinbase-samples/advanced-trade-sdk-go/client"
	"github.com/coinbase-samples/advanced-trade-sdk-go/credentials"
	"github.com/coinbase-samples/advanced-trade-sdk-go/model"
	"github.com/coinbase-samples/advanced-trade-sdk-go/orders"
	"github.com/coinbase-samples/advanced-trade-sdk-go/portfolios"
	"github.com/coinbase-samples/advanced-trade-sdk-go/products"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/benny-conn/brandon-bot/provider"
	"github.com/benny-conn/brandon-bot/strategy"
)

const (
	wsURL     = "wss://advanced-trade-ws.coinbase.com"
	wsUserURL = "wss://advanced-trade-ws-user.coinbase.com"
)

// Config holds Coinbase provider credentials.
type Config struct {
	AccessKey     string `json:"access_key"`
	PrivatePemKey string `json:"private_pem_key"`
}

// Provider implements provider.MarketData and provider.Execution for Coinbase.
type Provider struct {
	products   products.ProductsService
	orders     orders.OrdersService
	accounts   accounts.AccountsService
	portfolios portfolios.PortfoliosService
	accessKey  string
	pemKey     string
}

// New creates a Coinbase provider.
func New(cfg Config) *Provider {
	accessKey := envOr(cfg.AccessKey, "COINBASE_ACCESS_KEY")
	pemKey := envOr(cfg.PrivatePemKey, "COINBASE_PRIVATE_PEM_KEY")

	creds := &credentials.Credentials{
		AccessKey:     accessKey,
		PrivatePemKey: pemKey,
	}
	httpClient, err := cbclient.DefaultHttpClient()
	if err != nil {
		log.Fatalf("coinbase: creating http client: %v", err)
	}
	rc := cbclient.NewRestClient(creds, httpClient)

	return &Provider{
		products:   products.NewProductsService(rc),
		orders:     orders.NewOrdersService(rc),
		accounts:   accounts.NewAccountsService(rc),
		portfolios: portfolios.NewPortfoliosService(rc),
		accessKey:  accessKey,
		pemKey:     pemKey,
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
	gran, err := parseGranularity(timeframe)
	if err != nil {
		return nil, err
	}

	var allBars []provider.Bar
	chunkStart := start

	for chunkStart.Before(end) {
		// Max 300 candles per request.
		chunkEnd := end
		chunkDur := granularityDuration(timeframe) * 300
		if chunkStart.Add(chunkDur).Before(end) {
			chunkEnd = chunkStart.Add(chunkDur)
		}

		resp, err := p.products.GetProductCandles(ctx, &products.GetProductCandlesRequest{
			ProductId:   symbol,
			Start:       strconv.FormatInt(chunkStart.Unix(), 10),
			End:         strconv.FormatInt(chunkEnd.Unix(), 10),
			Granularity: gran,
		})
		if err != nil {
			return nil, fmt.Errorf("coinbase: fetching candles for %s: %w", symbol, err)
		}

		if resp.Candles != nil {
			for _, c := range *resp.Candles {
				bar, err := candleToBar(symbol, c)
				if err != nil {
					continue
				}
				allBars = append(allBars, bar)
			}
		}

		chunkStart = chunkEnd
	}

	sort.Slice(allBars, func(i, j int) bool {
		return allBars[i].Timestamp.Before(allBars[j].Timestamp)
	})
	return allBars, nil
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
	// Coinbase WS doesn't have a bar/candle channel.
	// Build bars from the trade stream by aggregating within timeframe windows.
	dur := granularityDuration(timeframe)

	type bucket struct {
		open, high, low, close, volume float64
		ts                             time.Time
		valid                          bool
	}
	buckets := make(map[string]*bucket)

	flush := func(sym string) {
		b := buckets[sym]
		if b != nil && b.valid {
			handler(provider.Bar{
				Symbol:    sym,
				Timestamp: b.ts,
				Open:      b.open,
				High:      b.high,
				Low:       b.low,
				Close:     b.close,
				Volume:    b.volume,
			})
		}
		buckets[sym] = nil
	}

	return p.subscribeWS(ctx, "market_trades", symbols, func(raw json.RawMessage) {
		var msg wsTrades
		if err := json.Unmarshal(raw, &msg); err != nil {
			return
		}
		for _, t := range msg.Trades {
			price, _ := strconv.ParseFloat(t.Price, 64)
			size, _ := strconv.ParseFloat(t.Size, 64)
			ts, _ := time.Parse(time.RFC3339Nano, t.Time)
			sym := t.ProductID
			win := ts.Truncate(dur)

			b := buckets[sym]
			if b != nil && b.valid && win != b.ts {
				flush(sym)
			}
			if buckets[sym] == nil {
				buckets[sym] = &bucket{
					open: price, high: price, low: price, close: price,
					volume: size, ts: win, valid: true,
				}
			} else {
				b = buckets[sym]
				if price > b.high {
					b.high = price
				}
				if price < b.low {
					b.low = price
				}
				b.close = price
				b.volume += size
			}
		}
	})
}

func (p *Provider) SubscribeTrades(ctx context.Context, symbols []string, handler func(provider.Trade)) error {
	return p.subscribeWS(ctx, "market_trades", symbols, func(raw json.RawMessage) {
		var msg wsTrades
		if err := json.Unmarshal(raw, &msg); err != nil {
			return
		}
		for _, t := range msg.Trades {
			price, _ := strconv.ParseFloat(t.Price, 64)
			size, _ := strconv.ParseFloat(t.Size, 64)
			ts, _ := time.Parse(time.RFC3339Nano, t.Time)
			handler(provider.Trade{
				Symbol:    t.ProductID,
				Timestamp: ts,
				Price:     price,
				Size:      size,
			})
		}
	})
}

func (p *Provider) SubscribeQuotes(ctx context.Context, symbols []string, handler func(provider.Quote)) error {
	return p.subscribeWS(ctx, "ticker", symbols, func(raw json.RawMessage) {
		var msg wsTicker
		if err := json.Unmarshal(raw, &msg); err != nil {
			return
		}
		for _, t := range msg.Tickers {
			bestBid, _ := strconv.ParseFloat(t.BestBid, 64)
			bestBidQty, _ := strconv.ParseFloat(t.BestBidQty, 64)
			bestAsk, _ := strconv.ParseFloat(t.BestAsk, 64)
			bestAskQty, _ := strconv.ParseFloat(t.BestAskQty, 64)
			handler(provider.Quote{
				Symbol:   t.ProductID,
				BidPrice: bestBid,
				BidSize:  bestBidQty,
				AskPrice: bestAsk,
				AskSize:  bestAskQty,
			})
		}
	})
}

// — Execution —

func (p *Provider) GetAccount(ctx context.Context) (provider.Account, error) {
	resp, err := p.accounts.ListAccounts(ctx, &accounts.ListAccountsRequest{})
	if err != nil {
		return provider.Account{}, fmt.Errorf("coinbase: listing accounts: %w", err)
	}

	var totalCash float64
	for _, acct := range resp.Accounts {
		if acct.Currency == "USD" || acct.Currency == "USDC" || acct.Currency == "USDT" {
			val, _ := strconv.ParseFloat(acct.AvailableBalance.Value, 64)
			totalCash += val
		}
	}

	// Portfolio breakdown for total equity.
	var equity float64
	portResp, err := p.portfolios.ListPortfolios(ctx, &portfolios.ListPortfoliosRequest{})
	if err == nil && len(portResp.Portfolios) > 0 {
		breakdown, err := p.portfolios.GetPortfolioBreakdown(ctx, &portfolios.GetPortfolioBreakdownRequest{
			PortfolioUuid: portResp.Portfolios[0].Uuid,
		})
		if err == nil && breakdown.Breakdown != nil {
			equity, _ = strconv.ParseFloat(breakdown.Breakdown.PortfolioBalances.TotalBalance.Value, 64)
		}
	}
	if equity == 0 {
		equity = totalCash
	}

	return provider.Account{Cash: totalCash, Equity: equity}, nil
}

func (p *Provider) GetPositions(ctx context.Context) ([]provider.Position, error) {
	portResp, err := p.portfolios.ListPortfolios(ctx, &portfolios.ListPortfoliosRequest{})
	if err != nil {
		return nil, fmt.Errorf("coinbase: listing portfolios: %w", err)
	}
	if len(portResp.Portfolios) == 0 {
		return nil, nil
	}

	breakdown, err := p.portfolios.GetPortfolioBreakdown(ctx, &portfolios.GetPortfolioBreakdownRequest{
		PortfolioUuid: portResp.Portfolios[0].Uuid,
	})
	if err != nil {
		return nil, fmt.Errorf("coinbase: portfolio breakdown: %w", err)
	}
	if breakdown.Breakdown == nil {
		return nil, nil
	}

	var positions []provider.Position
	for _, sp := range breakdown.Breakdown.SpotPositions {
		if sp.IsCash || sp.TotalBalanceCrypto == 0 {
			continue
		}
		var currentPrice float64
		if sp.TotalBalanceCrypto != 0 {
			currentPrice = sp.TotalBalanceFiat / sp.TotalBalanceCrypto
		}
		costBasis, _ := strconv.ParseFloat(sp.CostBasis.Value, 64)
		var avgEntry float64
		if sp.TotalBalanceCrypto != 0 {
			avgEntry = costBasis / sp.TotalBalanceCrypto
		}
		positions = append(positions, provider.Position{
			Symbol:        sp.Asset + "-USD",
			Qty:           sp.TotalBalanceCrypto,
			AvgEntryPrice: avgEntry,
			CurrentPrice:  currentPrice,
		})
	}
	return positions, nil
}

func (p *Provider) GetOpenOrders(ctx context.Context) ([]provider.OpenOrder, error) {
	resp, err := p.orders.ListOrders(ctx, &orders.ListOrdersRequest{
		OrderStatus: []string{"OPEN", "PENDING"},
	})
	if err != nil {
		return nil, fmt.Errorf("coinbase: listing open orders: %w", err)
	}

	result := make([]provider.OpenOrder, 0, len(resp.Orders))
	for _, o := range resp.Orders {
		filled, _ := strconv.ParseFloat(o.FilledSize, 64)
		oo := provider.OpenOrder{
			ID:     o.OrderId,
			Symbol: o.ProductId,
			Side:   strings.ToLower(o.Side),
			Filled: filled,
		}

		cfg := o.OrderConfiguration
		switch {
		case cfg.MarketMarketIoc != nil:
			oo.OrderType = "market"
			oo.Qty, _ = strconv.ParseFloat(cfg.MarketMarketIoc.BaseSize, 64)
		case cfg.LimitLimitGtc != nil:
			oo.OrderType = "limit"
			oo.Qty, _ = strconv.ParseFloat(cfg.LimitLimitGtc.BaseSize, 64)
			oo.LimitPrice, _ = strconv.ParseFloat(cfg.LimitLimitGtc.LimitPrice, 64)
		case cfg.StopLimitStopLimitGtc != nil:
			oo.OrderType = "stop_limit"
			oo.Qty, _ = strconv.ParseFloat(cfg.StopLimitStopLimitGtc.BaseSize, 64)
			oo.LimitPrice, _ = strconv.ParseFloat(cfg.StopLimitStopLimitGtc.LimitPrice, 64)
			oo.StopPrice, _ = strconv.ParseFloat(cfg.StopLimitStopLimitGtc.StopPrice, 64)
		}
		result = append(result, oo)
	}
	return result, nil
}

func (p *Provider) PlaceOrder(ctx context.Context, order strategy.Order) (provider.OrderResult, error) {
	side := "BUY"
	if order.Side == "sell" {
		side = "SELL"
	}

	req := &orders.CreateOrderRequest{
		ProductId:     order.Symbol,
		Side:          side,
		ClientOrderId: uuid.New().String(),
	}

	if order.OrderType == "limit" && order.LimitPrice > 0 {
		req.OrderConfiguration = model.OrderConfiguration{
			LimitLimitGtc: &model.LimitGtc{
				BaseSize:   strconv.FormatFloat(order.Qty, 'f', -1, 64),
				LimitPrice: strconv.FormatFloat(order.LimitPrice, 'f', -1, 64),
			},
		}
	} else {
		req.OrderConfiguration = model.OrderConfiguration{
			MarketMarketIoc: &model.MarketIoc{
				BaseSize: strconv.FormatFloat(order.Qty, 'f', -1, 64),
			},
		}
	}

	resp, err := p.orders.CreateOrder(ctx, req)
	if err != nil {
		return provider.OrderResult{}, fmt.Errorf("coinbase: placing %s order for %s qty=%.8f: %w",
			order.Side, order.Symbol, order.Qty, err)
	}
	if !resp.Success {
		reason := resp.FailureReason
		if resp.ErrorResponse != nil {
			reason = resp.ErrorResponse.Message
		}
		return provider.OrderResult{}, fmt.Errorf("coinbase: order rejected: %s", reason)
	}
	return provider.OrderResult{ID: resp.OrderId}, nil
}

func (p *Provider) CancelOrder(ctx context.Context, orderID string) error {
	resp, err := p.orders.CancelOrders(ctx, &orders.CancelOrdersRequest{
		OrderIds: []string{orderID},
	})
	if err != nil {
		return fmt.Errorf("coinbase: cancel order %s: %w", orderID, err)
	}
	if len(resp.Results) > 0 && !resp.Results[0].Success {
		return fmt.Errorf("coinbase: cancel order %s failed: %s", orderID, resp.Results[0].FailureReason)
	}
	return nil
}

func (p *Provider) SubscribeFills(ctx context.Context, handler func(provider.Fill)) error {
	return p.subscribeUserWS(ctx, "user", func(raw json.RawMessage) {
		var msg wsUserEvent
		if err := json.Unmarshal(raw, &msg); err != nil {
			return
		}
		for _, evt := range msg.Events {
			if evt.Type != "snapshot" && evt.Type != "update" {
				continue
			}
			for _, o := range evt.Orders {
				if o.Status != "FILLED" && o.Status != "PARTIALLY_FILLED" {
					continue
				}
				fillQty, _ := strconv.ParseFloat(o.FilledSize, 64)
				avgPrice, _ := strconv.ParseFloat(o.AvgPrice, 64)
				ts, _ := time.Parse(time.RFC3339Nano, o.LastFillTime)
				handler(provider.Fill{
					OrderID:   o.OrderID,
					Symbol:    o.ProductID,
					Side:      strings.ToLower(o.Side),
					Qty:       fillQty,
					Price:     avgPrice,
					Timestamp: ts,
					Partial:   o.Status == "PARTIALLY_FILLED",
				})
			}
		}
	})
}

// — WebSocket helpers —

func (p *Provider) generateWSJwt() (string, error) {
	block, _ := pem.Decode([]byte(p.pemKey))
	if block == nil {
		return "", fmt.Errorf("failed to parse PEM key")
	}
	privateKey, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parsing EC private key: %w", err)
	}

	now := time.Now()
	claims := jwt.MapClaims{
		"sub": p.accessKey,
		"iss": "coinbase-cloud",
		"nbf": now.Unix(),
		"exp": now.Add(2 * time.Minute).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["kid"] = p.accessKey
	token.Header["nonce"] = uuid.New().String()

	return token.SignedString(privateKey)
}

func (p *Provider) subscribeWS(ctx context.Context, channel string, productIDs []string, handler func(json.RawMessage)) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, http.Header{})
	if err != nil {
		return fmt.Errorf("coinbase: ws connect: %w", err)
	}
	defer conn.Close()

	if err := p.sendSubscribe(conn, channel, productIDs); err != nil {
		return err
	}

	// Re-auth every 90 seconds (JWT expires at 2 min).
	reauth := time.NewTicker(90 * time.Second)
	defer reauth.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-reauth.C:
			if err := p.sendSubscribe(conn, channel, productIDs); err != nil {
				log.Printf("coinbase: ws re-auth failed: %v", err)
			}
		default:
			_, msg, err := conn.ReadMessage()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("coinbase: ws read: %w", err)
			}
			var envelope wsEnvelope
			if err := json.Unmarshal(msg, &envelope); err != nil {
				continue
			}
			if envelope.Channel == channel {
				handler(msg)
			}
		}
	}
}

func (p *Provider) subscribeUserWS(ctx context.Context, channel string, handler func(json.RawMessage)) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsUserURL, http.Header{})
	if err != nil {
		return fmt.Errorf("coinbase: user ws connect: %w", err)
	}
	defer conn.Close()

	if err := p.sendSubscribe(conn, channel, nil); err != nil {
		return err
	}

	reauth := time.NewTicker(90 * time.Second)
	defer reauth.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-reauth.C:
			if err := p.sendSubscribe(conn, channel, nil); err != nil {
				log.Printf("coinbase: user ws re-auth failed: %v", err)
			}
		default:
			_, msg, err := conn.ReadMessage()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("coinbase: user ws read: %w", err)
			}
			var envelope wsEnvelope
			if err := json.Unmarshal(msg, &envelope); err != nil {
				continue
			}
			if envelope.Channel == channel {
				handler(msg)
			}
		}
	}
}

func (p *Provider) sendSubscribe(conn *websocket.Conn, channel string, productIDs []string) error {
	jwtToken, err := p.generateWSJwt()
	if err != nil {
		return fmt.Errorf("coinbase: generating ws jwt: %w", err)
	}

	sub := wsSubscribe{
		Type:       "subscribe",
		Channel:    channel,
		ProductIDs: productIDs,
		JWT:        jwtToken,
		Timestamp:  strconv.FormatInt(time.Now().Unix(), 10),
	}
	return conn.WriteJSON(sub)
}

// — WebSocket message types —

type wsSubscribe struct {
	Type       string   `json:"type"`
	Channel    string   `json:"channel"`
	ProductIDs []string `json:"product_ids,omitempty"`
	JWT        string   `json:"jwt"`
	Timestamp  string   `json:"timestamp"`
}

type wsEnvelope struct {
	Channel   string          `json:"channel"`
	Timestamp string          `json:"timestamp"`
	Events    json.RawMessage `json:"events"`
}

type wsTrades struct {
	Channel string `json:"channel"`
	Trades  []struct {
		TradeID   string `json:"trade_id"`
		ProductID string `json:"product_id"`
		Price     string `json:"price"`
		Size      string `json:"size"`
		Side      string `json:"side"`
		Time      string `json:"time"`
	} `json:"trades"`
}

type wsTicker struct {
	Channel string `json:"channel"`
	Tickers []struct {
		ProductID  string `json:"product_id"`
		Price      string `json:"price"`
		BestBid    string `json:"best_bid"`
		BestBidQty string `json:"best_bid_quantity"`
		BestAsk    string `json:"best_ask"`
		BestAskQty string `json:"best_ask_quantity"`
	} `json:"tickers"`
}

type wsUserEvent struct {
	Channel string `json:"channel"`
	Events  []struct {
		Type   string `json:"type"`
		Orders []struct {
			OrderID      string `json:"order_id"`
			ProductID    string `json:"product_id"`
			Side         string `json:"side"`
			Status       string `json:"status"`
			FilledSize   string `json:"cumulative_quantity"`
			AvgPrice     string `json:"avg_price"`
			LastFillTime string `json:"last_fill_time"`
		} `json:"orders"`
	} `json:"events"`
}

// — Helpers —

func candleToBar(symbol string, c model.Candle) (provider.Bar, error) {
	startUnix, err := strconv.ParseInt(c.Start, 10, 64)
	if err != nil {
		return provider.Bar{}, err
	}
	o, _ := strconv.ParseFloat(c.Open, 64)
	h, _ := strconv.ParseFloat(c.High, 64)
	l, _ := strconv.ParseFloat(c.Low, 64)
	cl, _ := strconv.ParseFloat(c.Close, 64)
	v, _ := strconv.ParseFloat(c.Volume, 64)
	return provider.Bar{
		Symbol:    symbol,
		Timestamp: time.Unix(startUnix, 0).UTC(),
		Open:      o,
		High:      h,
		Low:       l,
		Close:     cl,
		Volume:    v,
	}, nil
}

func parseGranularity(tf string) (string, error) {
	switch tf {
	case "1m":
		return "ONE_MINUTE", nil
	case "5m":
		return "FIVE_MINUTE", nil
	case "15m":
		return "FIFTEEN_MINUTE", nil
	case "30m":
		return "THIRTY_MINUTE", nil
	case "1h":
		return "ONE_HOUR", nil
	case "1d":
		return "ONE_DAY", nil
	default:
		return "", fmt.Errorf("unsupported timeframe %q — use 1m, 5m, 15m, 30m, 1h, or 1d", tf)
	}
}

func granularityDuration(tf string) time.Duration {
	switch tf {
	case "1s":
		return time.Second
	case "1m":
		return time.Minute
	case "5m":
		return 5 * time.Minute
	case "15m":
		return 15 * time.Minute
	case "30m":
		return 30 * time.Minute
	case "1h":
		return time.Hour
	case "1d":
		return 24 * time.Hour
	default:
		return time.Minute
	}
}
