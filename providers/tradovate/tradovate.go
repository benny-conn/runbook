package tradovate

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/benny-conn/brandon-bot/provider"
	"github.com/benny-conn/brandon-bot/strategy"
)

const (
	demoAPIWS = "wss://demo.tradovateapi.com/v1/websocket"
	livAPIWS  = "wss://live.tradovateapi.com/v1/websocket"
	demoMDWS  = "wss://md-demo.tradovateapi.com/v1/websocket"
	liveMDWS  = "wss://md.tradovateapi.com/v1/websocket"
)

// Provider implements provider.MarketData and provider.Execution using Tradovate.
//
// Tradovate uses two separate WebSocket connections with different tokens:
//   - Trading socket (orders, account, positions, fills)
//   - Market data socket (bars, quotes)
//
// Both require a 2.5-second heartbeat and token renewal every 85 minutes.
type Provider struct {
	auth       *authClient
	apiWS      string // trading websocket URL
	mdWS       string // market data websocket URL
	accountID  int64
	accountSpec string

	// contractID ↔ symbol cache (avoids repeated contract/item lookups)
	contractMu sync.RWMutex
	byID       map[int64]string // contractID → symbol
	bySymbol   map[string]int64 // symbol → contractID
}

// Config holds Tradovate API credentials and settings.
type Config struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	AppID      string `json:"app_id"`
	CID        string `json:"cid"`
	Sec        string `json:"sec"`
	DeviceID   string `json:"device_id"`
	AppVersion string `json:"app_version"`
	Demo       *bool  `json:"demo"` // pointer so false is distinguishable from unset
}

func envOr(val, envKey string) string {
	if val != "" {
		return val
	}
	return os.Getenv(envKey)
}

// New creates a Tradovate provider. Config fields override env vars where set.
// Defaults to demo mode unless Demo is explicitly set to false.
func New(cfg Config) *Provider {
	demo := true
	if cfg.Demo != nil {
		demo = *cfg.Demo
	} else if os.Getenv("TRADOVATE_DEMO") == "false" {
		demo = false
	}
	apiWS := livAPIWS
	mdWS := liveMDWS
	if demo {
		apiWS = demoAPIWS
		mdWS = demoMDWS
	}
	creds := authCreds{
		username:   envOr(cfg.Username, "TRADOVATE_USERNAME"),
		password:   envOr(cfg.Password, "TRADOVATE_PASSWORD"),
		appID:      envOr(cfg.AppID, "TRADOVATE_APP_ID"),
		appVersion: envOr(cfg.AppVersion, "TRADOVATE_APP_VERSION"),
		deviceID:   envOr(cfg.DeviceID, "TRADOVATE_DEVICE_ID"),
		cid:        envOr(cfg.CID, "TRADOVATE_CID"),
		sec:        envOr(cfg.Sec, "TRADOVATE_SEC"),
	}
	return &Provider{
		auth:     newAuthClient(demo, creds),
		apiWS:    apiWS,
		mdWS:     mdWS,
		byID:     make(map[int64]string),
		bySymbol: make(map[string]int64),
	}
}

// ensureConnected authenticates and resolves the account ID on first use.
func (p *Provider) ensureConnected(ctx context.Context) error {
	if p.accountID != 0 {
		return nil
	}
	if err := p.auth.authenticate(); err != nil {
		return err
	}
	p.auth.startRenewal(ctx)

	accessToken, _ := p.auth.tokens()
	conn, err := dialTradovate(ctx, p.apiWS, accessToken)
	if err != nil {
		return fmt.Errorf("tradovate trading ws: %w", err)
	}
	defer conn.Close()

	var accounts []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := conn.Request(ctx, "account/list", nil, &accounts); err != nil {
		return fmt.Errorf("tradovate account/list: %w", err)
	}
	if len(accounts) == 0 {
		return fmt.Errorf("tradovate: no accounts found")
	}
	p.accountID = accounts[0].ID
	p.accountSpec = accounts[0].Name
	log.Printf("tradovate: authenticated — account=%s (id=%d)", p.accountSpec, p.accountID)
	return nil
}

// — MarketData —

func (p *Provider) FetchBars(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]provider.Bar, error) {
	if err := p.ensureConnected(ctx); err != nil {
		return nil, err
	}
	_, mdToken := p.auth.tokens()
	conn, err := dialTradovate(ctx, p.mdWS, mdToken)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	elemSize, elemUnit, underlyingType := chartParams(timeframe)
	numBars := estimateBars(start, end, timeframe)

	var resp struct {
		Charts []struct {
			Bars []tvBar `json:"bars"`
		} `json:"charts"`
	}
	err = conn.Request(ctx, "md/getChart", map[string]any{
		"symbol": symbol,
		"chartDescription": map[string]any{
			"underlyingType":  underlyingType,
			"elementSize":     elemSize,
			"elementSizeUnit": elemUnit,
			"withHistogram":   false,
		},
		"timeRange": map[string]any{
			"asMuchAsElements": numBars,
			"closestTimestamp": end.UTC().Format(time.RFC3339),
		},
	}, &resp)
	if err != nil {
		return nil, fmt.Errorf("tradovate getChart %s: %w", symbol, err)
	}

	var bars []provider.Bar
	if len(resp.Charts) > 0 {
		for _, b := range resp.Charts[0].Bars {
			ts, err := time.Parse(time.RFC3339, b.Timestamp)
			if err != nil {
				continue
			}
			if ts.Before(start) || ts.After(end) {
				continue
			}
			bars = append(bars, provider.Bar{
				Symbol:    symbol,
				Timestamp: ts,
				Open:      b.Open,
				High:      b.High,
				Low:       b.Low,
				Close:     b.Close,
				Volume:    float64(b.UpVolume + b.DownVolume),
			})
		}
	}
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

// SubscribeBars streams live completed bars via the Tradovate market data WebSocket.
// Uses md/getChart with keepUpToDate semantics — push events arrive as bars complete.
func (p *Provider) SubscribeBars(ctx context.Context, symbols []string, timeframe string, handler func(provider.Bar)) error {
	if err := p.ensureConnected(ctx); err != nil {
		return err
	}
	_, mdToken := p.auth.tokens()
	conn, err := dialTradovate(ctx, p.mdWS, mdToken)
	if err != nil {
		return err
	}
	defer conn.Close()

	elemSize, elemUnit, underlyingType := chartParams(timeframe)
	subscribeAt := time.Now()

	// chartID → symbol: populated as initial responses arrive.
	chartToSym := make(map[int]string)

	// Subscribe to each symbol, capturing the chart ID from the initial response.
	for _, sym := range symbols {
		ch, reqID, err := conn.Send(ctx, "md/getChart", map[string]any{
			"symbol": sym,
			"chartDescription": map[string]any{
				"underlyingType":  underlyingType,
				"elementSize":     elemSize,
				"elementSizeUnit": elemUnit,
				"withHistogram":   false,
			},
			"timeRange": map[string]any{
				"asMuchAsElements": 1, // minimal backfill; we warm up separately
			},
		})
		if err != nil {
			return fmt.Errorf("tradovate subscribeBars %s: %w", sym, err)
		}

		// Wait for the initial response to capture the chart ID.
		select {
		case f := <-ch:
			conn.removeHandler(reqID)
			if f.Status != 200 {
				return fmt.Errorf("tradovate subscribeBars %s: status %d", sym, f.Status)
			}
			var d struct {
				Charts []struct {
					ID int `json:"id"`
				} `json:"charts"`
			}
			if err := json.Unmarshal(f.Data, &d); err == nil && len(d.Charts) > 0 {
				chartToSym[d.Charts[0].ID] = sym
			}
		case <-ctx.Done():
			return nil
		case <-time.After(15 * time.Second):
			return fmt.Errorf("tradovate subscribeBars %s: timeout waiting for chart ID", sym)
		}
	}

	log.Printf("tradovate: subscribed to bars for %v (timeframe=%s)", symbols, timeframe)

	for {
		select {
		case <-ctx.Done():
			return nil
		case frame := <-conn.EventCh:
			var payload struct {
				Charts []struct {
					ID   int     `json:"id"`
					Bars []tvBar `json:"bars"`
				} `json:"charts"`
			}
			if err := json.Unmarshal(frame.Data, &payload); err != nil {
				continue
			}
			for _, chart := range payload.Charts {
				sym, ok := chartToSym[chart.ID]
				if !ok {
					continue
				}
				for _, b := range chart.Bars {
					ts, err := time.Parse(time.RFC3339, b.Timestamp)
					if err != nil {
						continue
					}
					if ts.Before(subscribeAt) {
						continue
					}
					handler(provider.Bar{
						Symbol:    sym,
						Timestamp: ts,
						Open:      b.Open,
						High:      b.High,
						Low:       b.Low,
						Close:     b.Close,
						Volume:    float64(b.UpVolume + b.DownVolume),
					})
				}
			}
		}
	}
}

// SubscribeTrades streams real-time quote updates via md/subscribequote.
// Each quote update contains the last trade price and size.
func (p *Provider) SubscribeTrades(ctx context.Context, symbols []string, handler func(provider.Trade)) error {
	if err := p.ensureConnected(ctx); err != nil {
		return err
	}
	_, mdToken := p.auth.tokens()
	conn, err := dialTradovate(ctx, p.mdWS, mdToken)
	if err != nil {
		return err
	}
	defer conn.Close()

	// contractID → symbol: populated as subscriptions confirm.
	contractToSym := make(map[int64]string)

	for _, sym := range symbols {
		ch, reqID, err := conn.Send(ctx, "md/subscribequote", map[string]string{"symbol": sym})
		if err != nil {
			return fmt.Errorf("tradovate subscribeTrades %s: %w", sym, err)
		}
		select {
		case f := <-ch:
			conn.removeHandler(reqID)
			if f.Status != 200 {
				return fmt.Errorf("tradovate subscribeTrades %s: status %d", sym, f.Status)
			}
			// Response contains the contractId for this subscription.
			var d struct {
				Subscriptions []struct {
					ID int64 `json:"id"`
				} `json:"subscriptions"`
			}
			if err := json.Unmarshal(f.Data, &d); err == nil && len(d.Subscriptions) > 0 {
				contractToSym[d.Subscriptions[0].ID] = sym
			}
		case <-ctx.Done():
			return nil
		case <-time.After(10 * time.Second):
			return fmt.Errorf("tradovate subscribeTrades %s: timeout", sym)
		}
	}

	log.Printf("tradovate: subscribed to trades for %v", symbols)

	for {
		select {
		case <-ctx.Done():
			return nil
		case frame := <-conn.EventCh:
			var payload struct {
				Quotes []struct {
					ContractID int64 `json:"contractId"`
					Entries    struct {
						Trade struct {
							Price float64 `json:"price"`
							Size  float64 `json:"size"`
						} `json:"Trade"`
					} `json:"entries"`
				} `json:"quotes"`
			}
			if err := json.Unmarshal(frame.Data, &payload); err != nil {
				continue
			}
			for _, q := range payload.Quotes {
				sym, ok := contractToSym[q.ContractID]
				if !ok {
					continue
				}
				price := q.Entries.Trade.Price
				if price == 0 {
					continue
				}
				handler(provider.Trade{
					Symbol:    sym,
					Timestamp: time.Now().UTC(),
					Price:     price,
					Size:      q.Entries.Trade.Size,
				})
			}
		}
	}
}

// — Execution —

func (p *Provider) GetAccount(ctx context.Context) (provider.Account, error) {
	if err := p.ensureConnected(ctx); err != nil {
		return provider.Account{}, err
	}
	accessToken, _ := p.auth.tokens()
	conn, err := dialTradovate(ctx, p.apiWS, accessToken)
	if err != nil {
		return provider.Account{}, err
	}
	defer conn.Close()

	var balances []struct {
		Amount float64 `json:"amount"`
		OpenPL float64 `json:"openPnl"`
	}
	if err := conn.Request(ctx, "cashBalance/getcashbalancesnapshot",
		map[string]any{"accountId": p.accountID}, &balances); err != nil {
		return provider.Account{}, fmt.Errorf("tradovate cash balance: %w", err)
	}
	if len(balances) == 0 {
		return provider.Account{}, nil
	}
	return provider.Account{
		Cash:   balances[0].Amount,
		Equity: balances[0].Amount + balances[0].OpenPL,
	}, nil
}

func (p *Provider) GetPositions(ctx context.Context) ([]provider.Position, error) {
	if err := p.ensureConnected(ctx); err != nil {
		return nil, err
	}
	accessToken, _ := p.auth.tokens()
	conn, err := dialTradovate(ctx, p.apiWS, accessToken)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	var raw []struct {
		ContractID int64   `json:"contractId"`
		NetPos     float64 `json:"netPos"`
		AvgPrice   float64 `json:"avgPrice"`
		NetPrice   float64 `json:"netPrice"`
	}
	if err := conn.Request(ctx, "position/list", nil, &raw); err != nil {
		return nil, fmt.Errorf("tradovate position/list: %w", err)
	}

	positions := make([]provider.Position, 0, len(raw))
	for _, r := range raw {
		if r.NetPos == 0 {
			continue
		}
		sym := p.symbolFromID(r.ContractID)
		if sym == "" {
			// Look up via contract/item.
			var contract struct {
				Name string `json:"name"`
			}
			if err := conn.Request(ctx,
				fmt.Sprintf("contract/item?id=%d", r.ContractID), nil, &contract); err == nil {
				sym = contract.Name
				p.cacheContract(r.ContractID, sym)
			} else {
				sym = fmt.Sprintf("CONTRACT_%d", r.ContractID)
			}
		}
		positions = append(positions, provider.Position{
			Symbol:        sym,
			Qty:           r.NetPos,
			AvgEntryPrice: r.AvgPrice,
			CurrentPrice:  r.NetPrice,
		})
	}
	return positions, nil
}

func (p *Provider) PlaceOrder(ctx context.Context, order strategy.Order) (provider.OrderResult, error) {
	if err := p.ensureConnected(ctx); err != nil {
		return provider.OrderResult{}, err
	}
	accessToken, _ := p.auth.tokens()
	conn, err := dialTradovate(ctx, p.apiWS, accessToken)
	if err != nil {
		return provider.OrderResult{}, err
	}
	defer conn.Close()

	action := "Buy"
	if order.Side == "sell" {
		action = "Sell"
	}
	req := map[string]any{
		"accountId":   p.accountID,
		"accountSpec": p.accountSpec,
		"symbol":      order.Symbol,
		"action":      action,
		"orderType":   "Market",
		"orderQty":    order.Qty,
		"timeInForce": "Day",
		"isAutomated": true,
	}
	if order.OrderType == "limit" && order.LimitPrice > 0 {
		req["orderType"] = "Limit"
		req["timeInForce"] = "GTC"
		req["price"] = order.LimitPrice
	}

	var resp struct {
		OrderID int64 `json:"orderId"`
	}
	if err := conn.Request(ctx, "order/placeorder", req, &resp); err != nil {
		return provider.OrderResult{}, fmt.Errorf("tradovate placeOrder %s %s: %w",
			order.Side, order.Symbol, err)
	}
	return provider.OrderResult{ID: fmt.Sprintf("%d", resp.OrderID)}, nil
}

// SubscribeFills streams order execution reports via user/syncrequest on the trading WebSocket.
// This is WebSocket-native (not polling) — fills arrive within milliseconds of execution.
func (p *Provider) SubscribeFills(ctx context.Context, handler func(provider.Fill)) error {
	if err := p.ensureConnected(ctx); err != nil {
		return err
	}
	accessToken, _ := p.auth.tokens()
	conn, err := dialTradovate(ctx, p.apiWS, accessToken)
	if err != nil {
		return err
	}
	defer conn.Close()

	// user/syncrequest streams all account entity changes (fills, orders, positions).
	ch, reqID, err := conn.Send(ctx, "user/syncrequest",
		map[string]any{"users": []int64{p.auth.userID()}})
	if err != nil {
		return fmt.Errorf("tradovate user/syncrequest: %w", err)
	}
	select {
	case f := <-ch:
		conn.removeHandler(reqID)
		if f.Status != 200 {
			return fmt.Errorf("tradovate user/syncrequest: status %d", f.Status)
		}
	case <-time.After(15 * time.Second):
		return fmt.Errorf("tradovate user/syncrequest: timeout")
	case <-ctx.Done():
		return nil
	}

	seenFills := make(map[int64]struct{})

	for {
		select {
		case <-ctx.Done():
			return nil
		case frame := <-conn.EventCh:
			var event struct {
				ExecutionReports []struct {
					ID         int64   `json:"id"`
					OrderID    int64   `json:"orderId"`
					ContractID int64   `json:"contractId"`
					Action     string  `json:"action"` // "Buy" or "Sell"
					Qty        float64 `json:"qty"`
					Price      float64 `json:"price"`
				} `json:"executionReports"`
			}
			if err := json.Unmarshal(frame.Data, &event); err != nil {
				continue
			}
			for _, er := range event.ExecutionReports {
				if _, seen := seenFills[er.ID]; seen {
					continue
				}
				seenFills[er.ID] = struct{}{}

				sym := p.symbolFromID(er.ContractID)
				if sym == "" {
					sym = fmt.Sprintf("CONTRACT_%d", er.ContractID)
				}
				handler(provider.Fill{
					OrderID:   fmt.Sprintf("%d", er.OrderID),
					Symbol:    sym,
					Side:      strings.ToLower(er.Action),
					Qty:       er.Qty,
					Price:     er.Price,
					Timestamp: time.Now().UTC(),
				})
			}
		}
	}
}

func (p *Provider) SubscribeQuotes(ctx context.Context, symbols []string, handler func(provider.Quote)) error {
	if err := p.ensureConnected(ctx); err != nil {
		return err
	}
	_, mdToken := p.auth.tokens()
	conn, err := dialTradovate(ctx, p.mdWS, mdToken)
	if err != nil {
		return err
	}
	defer conn.Close()

	contractToSym := make(map[int64]string)

	for _, sym := range symbols {
		ch, reqID, err := conn.Send(ctx, "md/subscribequote", map[string]string{"symbol": sym})
		if err != nil {
			return fmt.Errorf("tradovate subscribeQuotes %s: %w", sym, err)
		}
		select {
		case f := <-ch:
			conn.removeHandler(reqID)
			if f.Status != 200 {
				return fmt.Errorf("tradovate subscribeQuotes %s: status %d", sym, f.Status)
			}
			var d struct {
				Subscriptions []struct {
					ID int64 `json:"id"`
				} `json:"subscriptions"`
			}
			if err := json.Unmarshal(f.Data, &d); err == nil && len(d.Subscriptions) > 0 {
				contractToSym[d.Subscriptions[0].ID] = sym
			}
		case <-ctx.Done():
			return nil
		case <-time.After(10 * time.Second):
			return fmt.Errorf("tradovate subscribeQuotes %s: timeout", sym)
		}
	}

	log.Printf("tradovate: subscribed to quotes for %v", symbols)

	for {
		select {
		case <-ctx.Done():
			return nil
		case frame := <-conn.EventCh:
			var payload struct {
				Quotes []struct {
					ContractID int64 `json:"contractId"`
					Entries    struct {
						Bid struct {
							Price float64 `json:"price"`
							Size  float64 `json:"size"`
						} `json:"Bid"`
						Ask struct {
							Price float64 `json:"price"`
							Size  float64 `json:"size"`
						} `json:"Ask"`
					} `json:"entries"`
				} `json:"quotes"`
			}
			if err := json.Unmarshal(frame.Data, &payload); err != nil {
				continue
			}
			for _, q := range payload.Quotes {
				sym, ok := contractToSym[q.ContractID]
				if !ok {
					continue
				}
				if q.Entries.Bid.Price == 0 && q.Entries.Ask.Price == 0 {
					continue
				}
				handler(provider.Quote{
					Symbol:    sym,
					Timestamp: time.Now().UTC(),
					BidPrice:  q.Entries.Bid.Price,
					BidSize:   q.Entries.Bid.Size,
					AskPrice:  q.Entries.Ask.Price,
					AskSize:   q.Entries.Ask.Size,
				})
			}
		}
	}
}

func (p *Provider) CancelOrder(ctx context.Context, orderID string) error {
	if err := p.ensureConnected(ctx); err != nil {
		return err
	}
	accessToken, _ := p.auth.tokens()
	conn, err := dialTradovate(ctx, p.apiWS, accessToken)
	if err != nil {
		return err
	}
	defer conn.Close()

	id, err := strconv.ParseInt(orderID, 10, 64)
	if err != nil {
		return fmt.Errorf("tradovate: invalid order ID %q: %w", orderID, err)
	}
	return conn.Request(ctx, "order/cancelorder", map[string]any{"orderId": id}, nil)
}

func (p *Provider) GetOpenOrders(ctx context.Context) ([]provider.OpenOrder, error) {
	if err := p.ensureConnected(ctx); err != nil {
		return nil, err
	}
	accessToken, _ := p.auth.tokens()
	conn, err := dialTradovate(ctx, p.apiWS, accessToken)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	var raw []struct {
		ID         int64   `json:"id"`
		ContractID int64   `json:"contractId"`
		Action     string  `json:"action"`
		OrdStatus  string  `json:"ordStatus"`
		OrdType    string  `json:"ordType"`
		PriceLimit float64 `json:"price"`
		PriceStop  float64 `json:"stopPrice"`
		OrderQty   float64 `json:"orderQty"`
		CumQty     float64 `json:"cumQty"`
	}
	if err := conn.Request(ctx, "order/list", nil, &raw); err != nil {
		return nil, fmt.Errorf("tradovate order/list: %w", err)
	}

	var orders []provider.OpenOrder
	for _, r := range raw {
		if r.OrdStatus != "Working" && r.OrdStatus != "PendingNew" {
			continue
		}
		sym := p.symbolFromID(r.ContractID)
		if sym == "" {
			sym = fmt.Sprintf("CONTRACT_%d", r.ContractID)
		}
		orders = append(orders, provider.OpenOrder{
			ID:         fmt.Sprintf("%d", r.ID),
			Symbol:     sym,
			Side:       strings.ToLower(r.Action),
			Qty:        r.OrderQty,
			Filled:     r.CumQty,
			OrderType:  strings.ToLower(r.OrdType),
			LimitPrice: r.PriceLimit,
			StopPrice:  r.PriceStop,
		})
	}
	return orders, nil
}

// GetContractSpec implements provider.ContractSpecProvider.
// It queries the Tradovate contract/find endpoint to get tick size and value.
func (p *Provider) GetContractSpec(ctx context.Context, symbol string) (provider.ContractSpec, error) {
	if err := p.ensureConnected(ctx); err != nil {
		return provider.ContractSpec{}, err
	}
	accessToken, _ := p.auth.tokens()
	conn, err := dialTradovate(ctx, p.apiWS, accessToken)
	if err != nil {
		return provider.ContractSpec{}, fmt.Errorf("tradovate ws for contract spec: %w", err)
	}
	defer conn.Close()

	var contract struct {
		ID               int64   `json:"id"`
		Name             string  `json:"name"`
		ContractMaturity int64   `json:"contractMaturityId"`
		ProviderTickSize float64 `json:"providerTickSize"`
	}
	if err := conn.Request(ctx, fmt.Sprintf("contract/find?name=%s", symbol), nil, &contract); err != nil {
		return provider.ContractSpec{}, fmt.Errorf("tradovate contract/find %s: %w", symbol, err)
	}
	p.cacheContract(contract.ID, symbol)

	// Look up the product to get BigPointValue (point value per full point move).
	var maturity struct {
		ProductID int64 `json:"productId"`
	}
	if err := conn.Request(ctx, fmt.Sprintf("contractMaturity/item?id=%d", contract.ContractMaturity), nil, &maturity); err != nil {
		return provider.ContractSpec{}, fmt.Errorf("tradovate contractMaturity lookup: %w", err)
	}

	var product struct {
		TickSize      float64 `json:"tickSize"`
		ValuePerPoint float64 `json:"valuePerPoint"`
	}
	if err := conn.Request(ctx, fmt.Sprintf("product/item?id=%d", maturity.ProductID), nil, &product); err != nil {
		return provider.ContractSpec{}, fmt.Errorf("tradovate product lookup: %w", err)
	}

	tickSize := product.TickSize
	if tickSize == 0 && contract.ProviderTickSize > 0 {
		tickSize = contract.ProviderTickSize
	}
	pointValue := product.ValuePerPoint
	tickValue := 0.0
	if tickSize > 0 && pointValue > 0 {
		tickValue = pointValue * tickSize
	}

	return provider.ContractSpec{
		Symbol:     symbol,
		TickSize:   tickSize,
		TickValue:  tickValue,
		PointValue: pointValue,
	}, nil
}

// — contract cache helpers —

func (p *Provider) cacheContract(id int64, sym string) {
	p.contractMu.Lock()
	p.byID[id] = sym
	p.bySymbol[sym] = id
	p.contractMu.Unlock()
}

func (p *Provider) symbolFromID(id int64) string {
	p.contractMu.RLock()
	defer p.contractMu.RUnlock()
	return p.byID[id]
}

// — Tradovate-specific types and helpers —

type tvBar struct {
	Timestamp  string  `json:"timestamp"`
	Open       float64 `json:"open"`
	High       float64 `json:"high"`
	Low        float64 `json:"low"`
	Close      float64 `json:"close"`
	UpVolume   int64   `json:"upVolume"`
	DownVolume int64   `json:"downVolume"`
}

// chartParams converts a canonical timeframe string to Tradovate chart description fields.
func chartParams(timeframe string) (elementSize int, elementSizeUnit, underlyingType string) {
	switch timeframe {
	case "1s":
		// Tradovate doesn't have 1-second bars natively — use tick bars (closest equivalent).
		// For sub-second strategy logic, use SubscribeTrades instead.
		return 1, "UnderlyingUnits", "Tick"
	case "1m":
		return 1, "MinuteValue", "MinuteBar"
	case "5m":
		return 5, "MinuteValue", "MinuteBar"
	case "15m":
		return 15, "MinuteValue", "MinuteBar"
	case "1h":
		return 60, "MinuteValue", "MinuteBar"
	case "1d":
		return 1, "DayValue", "DailyBar"
	default:
		return 1, "MinuteValue", "MinuteBar"
	}
}

// estimateBars estimates how many bars to request to cover start→end for the given timeframe.
func estimateBars(start, end time.Time, timeframe string) int {
	dur := end.Sub(start)
	days := dur.Hours() / 24
	var barsPerDay float64
	switch timeframe {
	case "1m":
		barsPerDay = 23 * 60 // futures trade ~23h/day
	case "5m":
		barsPerDay = 23 * 12
	case "15m":
		barsPerDay = 23 * 4
	case "1h":
		barsPerDay = 23
	default:
		barsPerDay = 1
	}
	n := int(days*barsPerDay) + 500
	if n > 50000 {
		n = 50000
	}
	return n
}
