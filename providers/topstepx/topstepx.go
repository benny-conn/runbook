// Package topstepx implements provider.MarketData and provider.Execution using the
// TopstepX (ProjectX) REST + SignalR API. Reads TOPSTEPX_USERNAME and TOPSTEPX_API_KEY from env.
package topstepx

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

// Config holds TopstepX API credentials.
type Config struct {
	Username string `json:"username"`
	APIKey   string `json:"api_key"`
}

// Provider implements provider.MarketData and provider.Execution using TopstepX.
type Provider struct {
	auth      *authClient
	accountID int64

	// contract cache: symbol → contract info
	contractMu sync.RWMutex
	contracts  map[string]*contractInfo
}

type contractInfo struct {
	ID        int64   // numeric ID used for history/bars
	StringID  string  // e.g. "CON.F.US.EP.U25" used for orders
	Symbol    string  // user-facing symbol e.g. "ES", "NQ"
	TickSize  float64
	TickValue float64
}

func envOr(val, envKey string) string {
	if val != "" {
		return val
	}
	return os.Getenv(envKey)
}

// New creates a TopstepX provider.
func New(cfg Config) *Provider {
	creds := authCreds{
		username: envOr(cfg.Username, "TOPSTEPX_USERNAME"),
		apiKey:   envOr(cfg.APIKey, "TOPSTEPX_API_KEY"),
	}
	return &Provider{
		auth:      newAuthClient(creds),
		contracts: make(map[string]*contractInfo),
	}
}

// ensureConnected authenticates and resolves the account on first use.
func (p *Provider) ensureConnected(ctx context.Context) error {
	if p.accountID != 0 {
		return nil
	}
	if err := p.auth.authenticate(); err != nil {
		return err
	}
	p.auth.startRenewal(ctx)

	var resp struct {
		Accounts []struct {
			ID       int64  `json:"id"`
			Name     string `json:"name"`
			CanTrade bool   `json:"canTrade"`
		} `json:"accounts"`
		Success bool `json:"success"`
	}
	if err := p.auth.doPost(ctx, "/Account/search", map[string]bool{"onlyActiveAccounts": true}, &resp); err != nil {
		return fmt.Errorf("topstepx account search: %w", err)
	}
	if !resp.Success || len(resp.Accounts) == 0 {
		return fmt.Errorf("topstepx: no active accounts found")
	}
	p.accountID = resp.Accounts[0].ID
	log.Printf("topstepx: authenticated — account=%s (id=%d)", resp.Accounts[0].Name, p.accountID)
	return nil
}

// resolveContract looks up a contract by symbol, caching the result.
func (p *Provider) resolveContract(ctx context.Context, symbol string) (*contractInfo, error) {
	p.contractMu.RLock()
	if c, ok := p.contracts[symbol]; ok {
		p.contractMu.RUnlock()
		return c, nil
	}
	p.contractMu.RUnlock()

	var resp struct {
		Contracts []struct {
			ID        int64   `json:"id"`
			ContractID string `json:"contractId"` // string format e.g. "CON.F.US.ENQ.U25"
			Name      string  `json:"name"`
			TickSize  float64 `json:"tickSize"`
			TickValue float64 `json:"tickValue"`
		} `json:"contracts"`
		Success bool `json:"success"`
	}
	if err := p.auth.doPost(ctx, "/Contract/search", map[string]any{
		"searchText": symbol,
		"live":       false,
	}, &resp); err != nil {
		return nil, fmt.Errorf("topstepx contract search %s: %w", symbol, err)
	}
	if !resp.Success || len(resp.Contracts) == 0 {
		return nil, fmt.Errorf("topstepx: contract not found for %q", symbol)
	}

	c := &contractInfo{
		ID:        resp.Contracts[0].ID,
		StringID:  resp.Contracts[0].ContractID,
		Symbol:    symbol,
		TickSize:  resp.Contracts[0].TickSize,
		TickValue: resp.Contracts[0].TickValue,
	}

	p.contractMu.Lock()
	p.contracts[symbol] = c
	p.contractMu.Unlock()

	return c, nil
}

// symbolFromContractID returns the cached symbol for a numeric contract ID.
func (p *Provider) symbolFromContractID(id int64) string {
	p.contractMu.RLock()
	defer p.contractMu.RUnlock()
	for _, c := range p.contracts {
		if c.ID == id {
			return c.Symbol
		}
	}
	return ""
}

// symbolFromStringID returns the cached symbol for a string contract ID.
func (p *Provider) symbolFromStringID(sid string) string {
	p.contractMu.RLock()
	defer p.contractMu.RUnlock()
	for _, c := range p.contracts {
		if c.StringID == sid {
			return c.Symbol
		}
	}
	return ""
}

// — MarketData —

func (p *Provider) FetchBars(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]provider.Bar, error) {
	if err := p.ensureConnected(ctx); err != nil {
		return nil, err
	}

	contract, err := p.resolveContract(ctx, symbol)
	if err != nil {
		return nil, err
	}

	unit, unitNumber := barParams(timeframe)

	var resp struct {
		Bars []struct {
			T string  `json:"t"` // ISO timestamp
			O float64 `json:"o"`
			H float64 `json:"h"`
			L float64 `json:"l"`
			C float64 `json:"c"`
			V float64 `json:"v"`
		} `json:"bars"`
		Success bool `json:"success"`
	}

	if err := p.auth.doPost(ctx, "/History/retrieveBars", map[string]any{
		"contractId":        contract.ID,
		"live":              false,
		"startTime":         start.UTC().Format(time.RFC3339),
		"endTime":           end.UTC().Format(time.RFC3339),
		"unit":              unit,
		"unitNumber":        unitNumber,
		"limit":             20000,
		"includePartialBar": false,
	}, &resp); err != nil {
		return nil, fmt.Errorf("topstepx fetchBars %s: %w", symbol, err)
	}

	bars := make([]provider.Bar, 0, len(resp.Bars))
	for _, b := range resp.Bars {
		ts, err := time.Parse(time.RFC3339, b.T)
		if err != nil {
			// Try alternate format.
			ts, err = time.Parse("2006-01-02T15:04:05", b.T)
			if err != nil {
				continue
			}
		}
		if ts.Before(start) || ts.After(end) {
			continue
		}
		bars = append(bars, provider.Bar{
			Symbol:    symbol,
			Timestamp: ts,
			Open:      b.O,
			High:      b.H,
			Low:       b.L,
			Close:     b.C,
			Volume:    b.V,
		})
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

// SubscribeBars builds bars from GatewayQuote events on the market hub.
// TopstepX doesn't push completed bars natively, so we aggregate from quote ticks.
func (p *Provider) SubscribeBars(ctx context.Context, symbols []string, timeframe string, handler func(provider.Bar)) error {
	if err := p.ensureConnected(ctx); err != nil {
		return err
	}

	hub, err := dialHub(ctx, marketHubURL, p.auth.getToken())
	if err != nil {
		return err
	}
	defer hub.Close()

	// Resolve contracts and subscribe to quotes for each symbol.
	contractToSym := make(map[string]string) // stringID → symbol
	for _, sym := range symbols {
		contract, err := p.resolveContract(ctx, sym)
		if err != nil {
			return err
		}
		contractToSym[contract.StringID] = sym
		if err := hub.Send(ctx, "SubscribeContractQuotes", contract.StringID); err != nil {
			return fmt.Errorf("topstepx subscribeBars %s: %w", sym, err)
		}
	}

	log.Printf("topstepx: subscribed to bars for %v (timeframe=%s)", symbols, timeframe)

	// Bar aggregation state per symbol.
	type barState struct {
		open, high, low, close float64
		volume                 float64
		start                  time.Time
		hasData                bool
	}
	interval := barInterval(timeframe)
	barStates := make(map[string]*barState)
	for _, sym := range symbols {
		barStates[sym] = &barState{}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-ticker.C:
			// Emit completed bars.
			now := time.Now().UTC()
			for sym, bs := range barStates {
				if !bs.hasData {
					continue
				}
				handler(provider.Bar{
					Symbol:    sym,
					Timestamp: now,
					Open:      bs.open,
					High:      bs.high,
					Low:       bs.low,
					Close:     bs.close,
					Volume:    bs.volume,
				})
				barStates[sym] = &barState{}
			}

		case msg := <-hub.EventCh:
			if msg.Target != "GatewayQuote" {
				continue
			}
			if len(msg.Arguments) < 2 {
				continue
			}
			var contractID string
			if err := json.Unmarshal(msg.Arguments[0], &contractID); err != nil {
				continue
			}
			sym, ok := contractToSym[contractID]
			if !ok {
				continue
			}
			var quote struct {
				LastPrice float64 `json:"lastPrice"`
				Volume    float64 `json:"volume"`
			}
			if err := json.Unmarshal(msg.Arguments[1], &quote); err != nil {
				continue
			}
			if quote.LastPrice == 0 {
				continue
			}

			bs := barStates[sym]
			if !bs.hasData {
				bs.open = quote.LastPrice
				bs.high = quote.LastPrice
				bs.low = quote.LastPrice
				bs.start = time.Now().UTC()
				bs.hasData = true
			}
			if quote.LastPrice > bs.high {
				bs.high = quote.LastPrice
			}
			if quote.LastPrice < bs.low {
				bs.low = quote.LastPrice
			}
			bs.close = quote.LastPrice
			bs.volume = quote.Volume
		}
	}
}

// SubscribeTrades streams individual trade prints via the market hub.
func (p *Provider) SubscribeTrades(ctx context.Context, symbols []string, handler func(provider.Trade)) error {
	if err := p.ensureConnected(ctx); err != nil {
		return err
	}

	hub, err := dialHub(ctx, marketHubURL, p.auth.getToken())
	if err != nil {
		return err
	}
	defer hub.Close()

	contractToSym := make(map[string]string)
	for _, sym := range symbols {
		contract, err := p.resolveContract(ctx, sym)
		if err != nil {
			return err
		}
		contractToSym[contract.StringID] = sym
		if err := hub.Send(ctx, "SubscribeContractTrades", contract.StringID); err != nil {
			return fmt.Errorf("topstepx subscribeTrades %s: %w", sym, err)
		}
	}

	log.Printf("topstepx: subscribed to trades for %v", symbols)

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg := <-hub.EventCh:
			if msg.Target != "GatewayTrade" {
				continue
			}
			if len(msg.Arguments) < 2 {
				continue
			}
			var contractID string
			if err := json.Unmarshal(msg.Arguments[0], &contractID); err != nil {
				continue
			}
			sym, ok := contractToSym[contractID]
			if !ok {
				continue
			}
			var trade struct {
				Price  float64 `json:"price"`
				Volume float64 `json:"volume"`
			}
			if err := json.Unmarshal(msg.Arguments[1], &trade); err != nil {
				continue
			}
			if trade.Price == 0 {
				continue
			}
			handler(provider.Trade{
				Symbol:    sym,
				Timestamp: time.Now().UTC(),
				Price:     trade.Price,
				Size:      trade.Volume,
			})
		}
	}
}

// SubscribeQuotes streams bid/ask updates via the market hub.
func (p *Provider) SubscribeQuotes(ctx context.Context, symbols []string, handler func(provider.Quote)) error {
	if err := p.ensureConnected(ctx); err != nil {
		return err
	}

	hub, err := dialHub(ctx, marketHubURL, p.auth.getToken())
	if err != nil {
		return err
	}
	defer hub.Close()

	contractToSym := make(map[string]string)
	for _, sym := range symbols {
		contract, err := p.resolveContract(ctx, sym)
		if err != nil {
			return err
		}
		contractToSym[contract.StringID] = sym
		if err := hub.Send(ctx, "SubscribeContractQuotes", contract.StringID); err != nil {
			return fmt.Errorf("topstepx subscribeQuotes %s: %w", sym, err)
		}
	}

	log.Printf("topstepx: subscribed to quotes for %v", symbols)

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg := <-hub.EventCh:
			if msg.Target != "GatewayQuote" {
				continue
			}
			if len(msg.Arguments) < 2 {
				continue
			}
			var contractID string
			if err := json.Unmarshal(msg.Arguments[0], &contractID); err != nil {
				continue
			}
			sym, ok := contractToSym[contractID]
			if !ok {
				continue
			}
			var quote struct {
				BestBid  float64 `json:"bestBid"`
				BestAsk  float64 `json:"bestAsk"`
				BidSize  float64 `json:"bestBidSize"`
				AskSize  float64 `json:"bestAskSize"`
			}
			if err := json.Unmarshal(msg.Arguments[1], &quote); err != nil {
				continue
			}
			handler(provider.Quote{
				Symbol:    sym,
				Timestamp: time.Now().UTC(),
				BidPrice:  quote.BestBid,
				BidSize:   quote.BidSize,
				AskPrice:  quote.BestAsk,
				AskSize:   quote.AskSize,
			})
		}
	}
}

// — Execution —

func (p *Provider) GetAccount(ctx context.Context) (provider.Account, error) {
	if err := p.ensureConnected(ctx); err != nil {
		return provider.Account{}, err
	}

	var resp struct {
		Accounts []struct {
			ID      int64   `json:"id"`
			Balance float64 `json:"balance"`
		} `json:"accounts"`
		Success bool `json:"success"`
	}
	if err := p.auth.doPost(ctx, "/Account/search", map[string]bool{"onlyActiveAccounts": true}, &resp); err != nil {
		return provider.Account{}, fmt.Errorf("topstepx getAccount: %w", err)
	}
	for _, a := range resp.Accounts {
		if a.ID == p.accountID {
			return provider.Account{
				Cash:   a.Balance,
				Equity: a.Balance,
			}, nil
		}
	}
	return provider.Account{}, fmt.Errorf("topstepx: account %d not found", p.accountID)
}

func (p *Provider) GetPositions(ctx context.Context) ([]provider.Position, error) {
	if err := p.ensureConnected(ctx); err != nil {
		return nil, err
	}

	var resp struct {
		Positions []struct {
			ContractID   string  `json:"contractId"`
			Size         float64 `json:"size"`
			AveragePrice float64 `json:"averagePrice"`
		} `json:"positions"`
		Success bool `json:"success"`
	}
	if err := p.auth.doPost(ctx, "/Position/searchOpen", map[string]int64{"accountId": p.accountID}, &resp); err != nil {
		return nil, fmt.Errorf("topstepx getPositions: %w", err)
	}

	positions := make([]provider.Position, 0, len(resp.Positions))
	for _, pos := range resp.Positions {
		if pos.Size == 0 {
			continue
		}
		sym := p.symbolFromStringID(pos.ContractID)
		if sym == "" {
			sym = pos.ContractID
		}
		positions = append(positions, provider.Position{
			Symbol:        sym,
			Qty:           pos.Size,
			AvgEntryPrice: pos.AveragePrice,
		})
	}
	return positions, nil
}

func (p *Provider) GetOpenOrders(ctx context.Context) ([]provider.OpenOrder, error) {
	if err := p.ensureConnected(ctx); err != nil {
		return nil, err
	}

	var resp struct {
		Orders []struct {
			ID         int64   `json:"id"`
			ContractID string  `json:"contractId"`
			Side       int     `json:"side"`       // 0=Buy, 1=Sell
			Type       int     `json:"type"`        // 1=Limit, 2=Market, 4=Stop
			Size       float64 `json:"size"`
			FillVolume float64 `json:"fillVolume"`
			LimitPrice float64 `json:"limitPrice"`
			StopPrice  float64 `json:"stopPrice"`
		} `json:"orders"`
		Success bool `json:"success"`
	}
	if err := p.auth.doPost(ctx, "/Order/searchOpen", map[string]int64{"accountId": p.accountID}, &resp); err != nil {
		return nil, fmt.Errorf("topstepx getOpenOrders: %w", err)
	}

	orders := make([]provider.OpenOrder, 0, len(resp.Orders))
	for _, o := range resp.Orders {
		sym := p.symbolFromStringID(o.ContractID)
		if sym == "" {
			sym = o.ContractID
		}
		orders = append(orders, provider.OpenOrder{
			ID:         strconv.FormatInt(o.ID, 10),
			Symbol:     sym,
			Side:       sideFromInt(o.Side),
			Qty:        o.Size,
			Filled:     o.FillVolume,
			OrderType:  orderTypeFromInt(o.Type),
			LimitPrice: o.LimitPrice,
			StopPrice:  o.StopPrice,
		})
	}
	return orders, nil
}

func (p *Provider) PlaceOrder(ctx context.Context, order strategy.Order) (provider.OrderResult, error) {
	if err := p.ensureConnected(ctx); err != nil {
		return provider.OrderResult{}, err
	}

	contract, err := p.resolveContract(ctx, order.Symbol)
	if err != nil {
		return provider.OrderResult{}, err
	}

	side := 0 // Buy
	if order.Side == "sell" {
		side = 1
	}

	req := map[string]any{
		"accountId":  p.accountID,
		"contractId": contract.StringID,
		"type":       orderTypeToInt(order.OrderType),
		"side":       side,
		"size":       int(order.Qty),
	}
	if order.OrderType == "limit" && order.LimitPrice > 0 {
		req["limitPrice"] = order.LimitPrice
	}
	if order.OrderType == "stop" && order.StopPrice > 0 {
		req["stopPrice"] = order.StopPrice
	}

	var resp struct {
		OrderID int64  `json:"orderId"`
		Success bool   `json:"success"`
		ErrorMessage string `json:"errorMessage"`
	}
	if err := p.auth.doPost(ctx, "/Order/place", req, &resp); err != nil {
		return provider.OrderResult{}, fmt.Errorf("topstepx placeOrder %s %s: %w",
			order.Side, order.Symbol, err)
	}
	if !resp.Success {
		return provider.OrderResult{}, fmt.Errorf("topstepx placeOrder: %s", resp.ErrorMessage)
	}
	return provider.OrderResult{ID: strconv.FormatInt(resp.OrderID, 10)}, nil
}

func (p *Provider) CancelOrder(ctx context.Context, orderID string) error {
	if err := p.ensureConnected(ctx); err != nil {
		return err
	}

	id, err := strconv.ParseInt(orderID, 10, 64)
	if err != nil {
		return fmt.Errorf("topstepx: invalid order ID %q: %w", orderID, err)
	}

	var resp struct {
		Success      bool   `json:"success"`
		ErrorMessage string `json:"errorMessage"`
	}
	if err := p.auth.doPost(ctx, "/Order/cancel", map[string]any{
		"accountId": p.accountID,
		"orderId":   id,
	}, &resp); err != nil {
		return fmt.Errorf("topstepx cancelOrder %s: %w", orderID, err)
	}
	if !resp.Success {
		return fmt.Errorf("topstepx cancelOrder: %s", resp.ErrorMessage)
	}
	return nil
}

// SubscribeFills streams fill events via the user hub's GatewayUserTrade events.
func (p *Provider) SubscribeFills(ctx context.Context, handler func(provider.Fill)) error {
	if err := p.ensureConnected(ctx); err != nil {
		return err
	}

	hub, err := dialHub(ctx, userHubURL, p.auth.getToken())
	if err != nil {
		return err
	}
	defer hub.Close()

	// Subscribe to trade (fill) events for our account.
	if err := hub.Send(ctx, "SubscribeTrades", p.accountID); err != nil {
		return fmt.Errorf("topstepx subscribeFills: %w", err)
	}

	// Also subscribe to order events to correlate fills with order sides.
	if err := hub.Send(ctx, "SubscribeOrders", p.accountID); err != nil {
		return fmt.Errorf("topstepx subscribeOrders: %w", err)
	}

	log.Printf("topstepx: subscribed to fills for account %d", p.accountID)

	// Track order sides for fill correlation.
	orderSides := make(map[int64]string) // orderID → "buy"/"sell"

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg := <-hub.EventCh:
			switch msg.Target {
			case "GatewayUserOrder":
				// Track order side for later fill correlation.
				if len(msg.Arguments) < 1 {
					continue
				}
				var order struct {
					ID   int64 `json:"id"`
					Side int   `json:"side"` // 0=Buy, 1=Sell
				}
				if err := json.Unmarshal(msg.Arguments[0], &order); err != nil {
					continue
				}
				orderSides[order.ID] = sideFromInt(order.Side)

			case "GatewayUserTrade":
				if len(msg.Arguments) < 1 {
					continue
				}
				var trade struct {
					ID         int64   `json:"id"`
					OrderID    int64   `json:"orderId"`
					ContractID string  `json:"contractId"`
					Price      float64 `json:"price"`
					Size       float64 `json:"size"`
					Side       int     `json:"side"`
					Voided     bool    `json:"voided"`
				}
				if err := json.Unmarshal(msg.Arguments[0], &trade); err != nil {
					continue
				}
				if trade.Voided || trade.Price == 0 {
					continue
				}

				sym := p.symbolFromStringID(trade.ContractID)
				if sym == "" {
					sym = trade.ContractID
				}

				// Determine side: prefer trade's own side field, fall back to order cache.
				side := sideFromInt(trade.Side)
				if side == "" {
					side = orderSides[trade.OrderID]
				}

				handler(provider.Fill{
					OrderID:   strconv.FormatInt(trade.OrderID, 10),
					Symbol:    sym,
					Side:      side,
					Qty:       trade.Size,
					Price:     trade.Price,
					Timestamp: time.Now().UTC(),
				})
			}
		}
	}
}

// — helpers —

func sideFromInt(s int) string {
	switch s {
	case 0:
		return "buy"
	case 1:
		return "sell"
	default:
		return ""
	}
}

func orderTypeFromInt(t int) string {
	switch t {
	case 1:
		return "limit"
	case 2:
		return "market"
	case 3:
		return "stop_limit"
	case 4:
		return "stop"
	case 5:
		return "trailing_stop"
	default:
		return "market"
	}
}

func orderTypeToInt(t string) int {
	switch strings.ToLower(t) {
	case "limit":
		return 1
	case "market", "":
		return 2
	case "stop_limit":
		return 3
	case "stop":
		return 4
	case "trailing_stop":
		return 5
	default:
		return 2
	}
}

// barParams converts a canonical timeframe to TopstepX History API params.
// unit: 1=Second, 2=Minute, 3=Hour, 4=Day
func barParams(timeframe string) (unit, unitNumber int) {
	switch timeframe {
	case "1s":
		return 1, 1
	case "1m":
		return 2, 1
	case "5m":
		return 2, 5
	case "15m":
		return 2, 15
	case "1h":
		return 3, 1
	case "1d":
		return 4, 1
	default:
		return 2, 1
	}
}

// barInterval returns the duration for a timeframe string.
func barInterval(timeframe string) time.Duration {
	switch timeframe {
	case "1s":
		return time.Second
	case "1m":
		return time.Minute
	case "5m":
		return 5 * time.Minute
	case "15m":
		return 15 * time.Minute
	case "1h":
		return time.Hour
	case "1d":
		return 24 * time.Hour
	default:
		return time.Minute
	}
}
