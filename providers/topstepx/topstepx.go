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

// Config holds TopstepX API credentials and optional account selection.
type Config struct {
	Username  string `json:"username"`
	APIKey    string `json:"api_key"`
	AccountID int64  `json:"account_id,omitempty"` // optional: skip auto-select
}

// Provider implements provider.MarketData and provider.Execution using TopstepX.
type Provider struct {
	auth      *authClient
	accountID int64

	// contract cache: symbol → contract info
	contractMu sync.RWMutex
	contracts  map[string]*contractInfo

	// Shared hub connections — lazily initialized, auto-reconnecting.
	hubMu     sync.Mutex
	marketHub *managedHub
	userHub   *managedHub
}

type contractInfo struct {
	ID        string  // e.g. "CON.F.US.EP.M26" — used for all API calls
	Symbol    string  // user-facing symbol e.g. "ES", "NQ"
	TickSize  float64
	TickValue float64
}

// AccountInfo describes a TopstepX trading account.
type AccountInfo struct {
	ID       int64
	Name     string
	Balance  float64
	CanTrade bool
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
		accountID: cfg.AccountID,
		contracts: make(map[string]*contractInfo),
	}
}

// Close shuts down shared hub connections.
func (p *Provider) Close() {
	p.hubMu.Lock()
	defer p.hubMu.Unlock()
	if p.marketHub != nil {
		p.marketHub.close()
	}
	if p.userHub != nil {
		p.userHub.close()
	}
}

// ensureConnected authenticates and resolves the account on first use.
// If accountID was set via Config or SetAccount, it skips auto-selection.
func (p *Provider) ensureConnected(ctx context.Context) error {
	if p.auth.getToken() != "" {
		return nil // already authenticated
	}
	if err := p.auth.authenticate(); err != nil {
		return err
	}
	p.auth.startRenewal(ctx)

	// Register token-refresh handler to force hub reconnections.
	p.auth.onRefresh(func() {
		p.hubMu.Lock()
		defer p.hubMu.Unlock()
		// Close current connections; managedHub.run() will reconnect with new token.
		if p.marketHub != nil && p.marketHub.current != nil {
			p.marketHub.current.Close()
		}
		if p.userHub != nil && p.userHub.current != nil {
			p.userHub.current.Close()
		}
		log.Println("topstepx: token refreshed — hub reconnection triggered")
	})

	// If account was pre-selected via config or SetAccount, keep it.
	if p.accountID != 0 {
		log.Printf("topstepx: authenticated — using configured account id=%d", p.accountID)
		return nil
	}

	// Auto-select first active account.
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
	log.Printf("topstepx: authenticated — auto-selected account=%s (id=%d)", resp.Accounts[0].Name, p.accountID)
	return nil
}

// Authenticate explicitly authenticates without selecting an account.
// Useful for testing credentials before calling ListAccounts.
func (p *Provider) Authenticate(ctx context.Context) error {
	if err := p.auth.authenticate(); err != nil {
		return err
	}
	p.auth.startRenewal(ctx)
	return nil
}

// ListAccounts returns all active accounts for the authenticated user.
func (p *Provider) ListAccounts(ctx context.Context) ([]AccountInfo, error) {
	var resp struct {
		Accounts []struct {
			ID       int64   `json:"id"`
			Name     string  `json:"name"`
			Balance  float64 `json:"balance"`
			CanTrade bool    `json:"canTrade"`
		} `json:"accounts"`
		Success bool `json:"success"`
	}
	if err := p.auth.doPost(ctx, "/Account/search", map[string]bool{"onlyActiveAccounts": true}, &resp); err != nil {
		return nil, fmt.Errorf("topstepx list accounts: %w", err)
	}
	if !resp.Success {
		return nil, fmt.Errorf("topstepx: account search failed")
	}

	accounts := make([]AccountInfo, len(resp.Accounts))
	for i, a := range resp.Accounts {
		accounts[i] = AccountInfo{
			ID:       a.ID,
			Name:     a.Name,
			Balance:  a.Balance,
			CanTrade: a.CanTrade,
		}
	}
	return accounts, nil
}

// SetAccount selects a specific account by ID for all subsequent operations.
func (p *Provider) SetAccount(accountID int64) {
	p.accountID = accountID
}

// AccountID returns the currently selected account ID (0 if none selected).
func (p *Provider) AccountID() int64 {
	return p.accountID
}

// --- shared hub management ---

// getMarketHub returns the shared market hub, starting it on first call.
func (p *Provider) getMarketHub(ctx context.Context) *managedHub {
	p.hubMu.Lock()
	defer p.hubMu.Unlock()
	if p.marketHub == nil {
		p.marketHub = newManagedHub(marketHubURL, p.auth.getToken)
		go p.marketHub.run(ctx)
	}
	return p.marketHub
}

// getUserHub returns the shared user hub, starting it on first call.
func (p *Provider) getUserHub(ctx context.Context) *managedHub {
	p.hubMu.Lock()
	defer p.hubMu.Unlock()
	if p.userHub == nil {
		p.userHub = newManagedHub(userHubURL, p.auth.getToken)
		go p.userHub.run(ctx)
	}
	return p.userHub
}

// --- contract resolution ---

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
			ID        string  `json:"id"`
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
		Symbol:    symbol,
		TickSize:  resp.Contracts[0].TickSize,
		TickValue: resp.Contracts[0].TickValue,
	}

	p.contractMu.Lock()
	p.contracts[symbol] = c
	p.contractMu.Unlock()

	return c, nil
}

// symbolFromContractID returns the cached symbol for a contract ID string.
func (p *Provider) symbolFromContractID(id string) string {
	p.contractMu.RLock()
	defer p.contractMu.RUnlock()
	for _, c := range p.contracts {
		if c.ID == id {
			return c.Symbol
		}
	}
	return ""
}

// --- MarketData ---

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

// --- quote delta handling ---

// gatewayQuote is the raw GatewayQuote payload from TopstepX.
// Fields are pointers so we can distinguish absent (delta) from zero.
type gatewayQuote struct {
	LastPrice     *float64 `json:"lastPrice"`
	BestBid       *float64 `json:"bestBid"`
	BestAsk       *float64 `json:"bestAsk"`
	Volume        *float64 `json:"volume"`
	Open          *float64 `json:"open"`
	High          *float64 `json:"high"`
	Low           *float64 `json:"low"`
	Change        *float64 `json:"change"`
	ChangePercent *float64 `json:"changePercent"`
	Timestamp     string   `json:"timestamp"`   // exchange time
	LastUpdated   string   `json:"lastUpdated"` // server time
}

// quoteState tracks the last-known full state for a symbol, since TopstepX
// sends delta updates that only include changed fields.
type quoteState struct {
	lastPrice float64
	bestBid   float64
	bestAsk   float64
	volume    float64
	timestamp time.Time
}

// merge applies a delta quote onto the current state.
func (s *quoteState) merge(q *gatewayQuote) {
	if q.LastPrice != nil {
		s.lastPrice = *q.LastPrice
	}
	if q.BestBid != nil {
		s.bestBid = *q.BestBid
	}
	if q.BestAsk != nil {
		s.bestAsk = *q.BestAsk
	}
	if q.Volume != nil {
		s.volume = *q.Volume
	}
	if q.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339Nano, q.Timestamp); err == nil {
			s.timestamp = t
		} else if t, err := time.Parse("2006-01-02T15:04:05.999999999+00:00", q.Timestamp); err == nil {
			s.timestamp = t
		}
	}
}

// parseQuoteEvent extracts the contract ID and delta quote from a GatewayQuote message.
func parseQuoteEvent(msg signalrMsg) (contractID string, q gatewayQuote, ok bool) {
	if msg.Target != "GatewayQuote" || len(msg.Arguments) < 2 {
		return "", gatewayQuote{}, false
	}
	if err := json.Unmarshal(msg.Arguments[0], &contractID); err != nil {
		return "", gatewayQuote{}, false
	}
	if err := json.Unmarshal(msg.Arguments[1], &q); err != nil {
		return "", gatewayQuote{}, false
	}
	return contractID, q, true
}

// --- streaming subscriptions (shared hub) ---

// SubscribeBars builds bars from GatewayQuote events on the shared market hub.
// TopstepX doesn't push completed bars natively, so we aggregate from quote ticks.
func (p *Provider) SubscribeBars(ctx context.Context, symbols []string, timeframe string, handler func(provider.Bar)) error {
	if err := p.ensureConnected(ctx); err != nil {
		return err
	}

	hub := p.getMarketHub(ctx)

	// Build contract mapping and subscribe.
	contractToSym := make(map[string]string)
	for _, sym := range symbols {
		contract, err := p.resolveContract(ctx, sym)
		if err != nil {
			return err
		}
		contractToSym[contract.ID] = sym
		if err := hub.subscribe(ctx, "SubscribeContractQuotes", contract.ID); err != nil {
			return fmt.Errorf("topstepx subscribeBars %s: %w", sym, err)
		}
	}

	// Local event channel for this consumer.
	ch := make(chan signalrMsg, 512)
	handlerID := hub.addHandler(func(msg signalrMsg) {
		if msg.Target != "GatewayQuote" {
			return
		}
		select {
		case ch <- msg:
		default:
		}
	})
	defer hub.removeHandler(handlerID)

	log.Printf("topstepx: subscribed to bars for %v (timeframe=%s)", symbols, timeframe)

	// Bar aggregation state per symbol.
	type barState struct {
		open, high, low, close float64
		volume                 float64
		hasData                bool
	}
	interval := barInterval(timeframe)
	barStates := make(map[string]*barState)
	quoteStates := make(map[string]*quoteState)
	for _, sym := range symbols {
		barStates[sym] = &barState{}
		quoteStates[sym] = &quoteState{}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-ticker.C:
			for sym, bs := range barStates {
				if !bs.hasData {
					continue
				}
				ts := quoteStates[sym].timestamp
				if ts.IsZero() {
					ts = time.Now().UTC()
				}
				handler(provider.Bar{
					Symbol:    sym,
					Timestamp: ts,
					Open:      bs.open,
					High:      bs.high,
					Low:       bs.low,
					Close:     bs.close,
					Volume:    bs.volume,
				})
				barStates[sym] = &barState{}
			}

		case msg := <-ch:
			contractID, quote, ok := parseQuoteEvent(msg)
			if !ok {
				continue
			}
			sym, ok := contractToSym[contractID]
			if !ok {
				continue
			}

			qs := quoteStates[sym]
			qs.merge(&quote)

			if qs.lastPrice == 0 {
				continue
			}

			bs := barStates[sym]
			if !bs.hasData {
				bs.open = qs.lastPrice
				bs.high = qs.lastPrice
				bs.low = qs.lastPrice
				bs.hasData = true
			}
			if qs.lastPrice > bs.high {
				bs.high = qs.lastPrice
			}
			if qs.lastPrice < bs.low {
				bs.low = qs.lastPrice
			}
			bs.close = qs.lastPrice
			bs.volume = qs.volume
		}
	}
}

// SubscribeTrades streams individual trade prints via the shared market hub.
func (p *Provider) SubscribeTrades(ctx context.Context, symbols []string, handler func(provider.Trade)) error {
	if err := p.ensureConnected(ctx); err != nil {
		return err
	}

	hub := p.getMarketHub(ctx)

	contractToSym := make(map[string]string)
	for _, sym := range symbols {
		contract, err := p.resolveContract(ctx, sym)
		if err != nil {
			return err
		}
		contractToSym[contract.ID] = sym
		if err := hub.subscribe(ctx, "SubscribeContractTrades", contract.ID); err != nil {
			return fmt.Errorf("topstepx subscribeTrades %s: %w", sym, err)
		}
	}

	ch := make(chan signalrMsg, 512)
	handlerID := hub.addHandler(func(msg signalrMsg) {
		if msg.Target != "GatewayTrade" {
			return
		}
		select {
		case ch <- msg:
		default:
		}
	})
	defer hub.removeHandler(handlerID)

	log.Printf("topstepx: subscribed to trades for %v", symbols)

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg := <-ch:
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
				Price     float64 `json:"price"`
				Volume    float64 `json:"volume"`
				Timestamp string  `json:"timestamp"`
			}
			if err := json.Unmarshal(msg.Arguments[1], &trade); err != nil {
				continue
			}
			if trade.Price == 0 {
				continue
			}
			ts := time.Now().UTC()
			if trade.Timestamp != "" {
				if parsed, err := time.Parse(time.RFC3339Nano, trade.Timestamp); err == nil {
					ts = parsed
				}
			}
			handler(provider.Trade{
				Symbol:    sym,
				Timestamp: ts,
				Price:     trade.Price,
				Size:      trade.Volume,
			})
		}
	}
}

// SubscribeQuotes streams bid/ask updates via the shared market hub.
// Handles TopstepX delta updates by tracking last-known state per symbol.
func (p *Provider) SubscribeQuotes(ctx context.Context, symbols []string, handler func(provider.Quote)) error {
	if err := p.ensureConnected(ctx); err != nil {
		return err
	}

	hub := p.getMarketHub(ctx)

	contractToSym := make(map[string]string)
	for _, sym := range symbols {
		contract, err := p.resolveContract(ctx, sym)
		if err != nil {
			return err
		}
		contractToSym[contract.ID] = sym
		if err := hub.subscribe(ctx, "SubscribeContractQuotes", contract.ID); err != nil {
			return fmt.Errorf("topstepx subscribeQuotes %s: %w", sym, err)
		}
	}

	ch := make(chan signalrMsg, 512)
	handlerID := hub.addHandler(func(msg signalrMsg) {
		if msg.Target != "GatewayQuote" {
			return
		}
		select {
		case ch <- msg:
		default:
		}
	})
	defer hub.removeHandler(handlerID)

	log.Printf("topstepx: subscribed to quotes for %v", symbols)

	states := make(map[string]*quoteState)
	for _, sym := range symbols {
		states[sym] = &quoteState{}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg := <-ch:
			contractID, quote, ok := parseQuoteEvent(msg)
			if !ok {
				continue
			}
			sym, ok := contractToSym[contractID]
			if !ok {
				continue
			}

			qs := states[sym]
			qs.merge(&quote)

			handler(provider.Quote{
				Symbol:    sym,
				Timestamp: qs.timestamp,
				BidPrice:  qs.bestBid,
				AskPrice:  qs.bestAsk,
			})
		}
	}
}

// --- Execution ---

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
		sym := p.symbolFromContractID(pos.ContractID)
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
			Type       int     `json:"type"`       // 1=Limit, 2=Market, 4=Stop
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
		sym := p.symbolFromContractID(o.ContractID)
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
		"contractId": contract.ID,
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
		OrderID      int64  `json:"orderId"`
		Success      bool   `json:"success"`
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

// SubscribeFills streams fill events via the shared user hub's GatewayUserTrade events.
func (p *Provider) SubscribeFills(ctx context.Context, handler func(provider.Fill)) error {
	if err := p.ensureConnected(ctx); err != nil {
		return err
	}

	hub := p.getUserHub(ctx)

	// Subscribe to trade (fill) and order events for our account.
	if err := hub.subscribe(ctx, "SubscribeTrades", p.accountID); err != nil {
		return fmt.Errorf("topstepx subscribeFills: %w", err)
	}
	if err := hub.subscribe(ctx, "SubscribeOrders", p.accountID); err != nil {
		return fmt.Errorf("topstepx subscribeOrders: %w", err)
	}

	ch := make(chan signalrMsg, 512)
	handlerID := hub.addHandler(func(msg signalrMsg) {
		if msg.Target != "GatewayUserTrade" && msg.Target != "GatewayUserOrder" {
			return
		}
		select {
		case ch <- msg:
		default:
		}
	})
	defer hub.removeHandler(handlerID)

	log.Printf("topstepx: subscribed to fills for account %d", p.accountID)

	// Track order sides for fill correlation.
	orderSides := make(map[int64]string) // orderID → "buy"/"sell"

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg := <-ch:
			switch msg.Target {
			case "GatewayUserOrder":
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

				sym := p.symbolFromContractID(trade.ContractID)
				if sym == "" {
					sym = trade.ContractID
				}

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

// --- helpers ---

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

// RawMarketEvent is an unprocessed SignalR market event for debugging.
type RawMarketEvent struct {
	Target    string            // e.g. "GatewayQuote", "GatewayTrade"
	Arguments []json.RawMessage // raw JSON payloads
}

// SubscribeRawMarketEvents streams raw SignalR events for the given symbols.
// Uses a direct (non-shared) connection for debugging purposes.
func (p *Provider) SubscribeRawMarketEvents(ctx context.Context, symbols []string, handler func(RawMarketEvent)) error {
	if p.auth.getToken() == "" {
		if err := p.auth.authenticate(); err != nil {
			return err
		}
	}

	hub, err := dialHub(ctx, marketHubURL, p.auth.getToken())
	if err != nil {
		return err
	}
	defer hub.Close()

	for _, sym := range symbols {
		contract, err := p.resolveContract(ctx, sym)
		if err != nil {
			return err
		}
		if err := hub.Send(ctx, "SubscribeContractQuotes", contract.ID); err != nil {
			return fmt.Errorf("topstepx subscribeRaw quotes %s: %w", sym, err)
		}
		if err := hub.Send(ctx, "SubscribeContractTrades", contract.ID); err != nil {
			return fmt.Errorf("topstepx subscribeRaw trades %s: %w", sym, err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg := <-hub.EventCh:
			handler(RawMarketEvent{
				Target:    msg.Target,
				Arguments: msg.Arguments,
			})
		}
	}
}
