package rithmic

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/benny-conn/brandon-bot/provider"
	"github.com/benny-conn/brandon-bot/strategy"
)

const (
	appName    = "brandon-bot"
	appVersion = "1.0.0"
)

// Config holds credentials for a Rithmic connection.
type Config struct {
	URI        string `json:"uri"`         // e.g. "wss://rituz00100.rithmic.com:443"
	SystemName string `json:"system_name"` // e.g. "Rithmic Test", "TopStep", etc.
	Username   string `json:"username"`
	Password   string `json:"password"`
	AccountID  string `json:"account_id,omitempty"` // optional, auto-selects first
	Exchange   string `json:"exchange,omitempty"`    // default "CME"
}

// Provider implements provider.MarketData, provider.Execution,
// provider.SessionNotifier, and provider.ContractSpecProvider.
type Provider struct {
	cfg Config

	mu          sync.Mutex
	tickerConn  *conn // TICKER_PLANT — market data
	orderConn   *conn // ORDER_PLANT — orders, accounts
	histConn    *conn // HISTORY_PLANT — bar replay

	// Resolved after ORDER_PLANT login
	fcmID     string
	ibID      string
	accountID string

	// Trade routes cache: exchange → route
	routesMu sync.RWMutex
	routes   map[string]string

	// Quote state for delta merging
	quotesMu sync.RWMutex
	quotes   map[string]*quoteState
}

type quoteState struct {
	bidPrice  float64
	bidSize   float64
	askPrice  float64
	askSize   float64
	lastPrice float64
	volume    float64
	ts        time.Time
}

func envOr(val, envKey string) string {
	if val != "" {
		return val
	}
	return os.Getenv(envKey)
}

// New creates a new Rithmic provider from config.
func New(cfg Config) *Provider {
	cfg.URI = envOr(cfg.URI, "RITHMIC_URI")
	cfg.SystemName = envOr(cfg.SystemName, "RITHMIC_SYSTEM_NAME")
	cfg.Username = envOr(cfg.Username, "RITHMIC_USERNAME")
	cfg.Password = envOr(cfg.Password, "RITHMIC_PASSWORD")
	cfg.AccountID = envOr(cfg.AccountID, "RITHMIC_ACCOUNT_ID")
	cfg.Exchange = envOr(cfg.Exchange, "RITHMIC_EXCHANGE")

	if cfg.URI == "" {
		cfg.URI = "wss://rituz00100.rithmic.com:443"
	}
	if cfg.Exchange == "" {
		cfg.Exchange = "CME"
	}

	return &Provider{
		cfg:    cfg,
		routes: make(map[string]string),
		quotes: make(map[string]*quoteState),
	}
}

// Close shuts down all connections.
func (p *Provider) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tickerConn != nil {
		p.tickerConn.close()
	}
	if p.orderConn != nil {
		p.orderConn.close()
	}
	if p.histConn != nil {
		p.histConn.close()
	}
}

// ---------------------------------------------------------------------------
// Connection management — lazy init per plant
// ---------------------------------------------------------------------------

func (p *Provider) getTickerConn(ctx context.Context) (*conn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tickerConn != nil {
		return p.tickerConn, nil
	}
	c, err := dial(ctx, p.cfg.URI, p.cfg.Username, p.cfg.Password, appName, appVersion, p.cfg.SystemName, infraTickerPlant)
	if err != nil {
		return nil, err
	}
	p.tickerConn = c
	return c, nil
}

func (p *Provider) getOrderConn(ctx context.Context) (*conn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.orderConn != nil {
		return p.orderConn, nil
	}
	c, err := dial(ctx, p.cfg.URI, p.cfg.Username, p.cfg.Password, appName, appVersion, p.cfg.SystemName, infraOrderPlant)
	if err != nil {
		return nil, err
	}
	p.orderConn = c
	p.fcmID = c.fcmID
	p.ibID = c.ibID

	// Resolve account
	if p.cfg.AccountID != "" {
		p.accountID = p.cfg.AccountID
	} else {
		acctID, err := p.resolveFirstAccount(ctx, c)
		if err != nil {
			c.close()
			p.orderConn = nil
			return nil, err
		}
		p.accountID = acctID
	}

	// Resolve trade routes
	if err := p.resolveTradeRoutes(ctx, c); err != nil {
		log.Printf("rithmic: warning: could not resolve trade routes: %v", err)
	}

	return c, nil
}

func (p *Provider) getHistConn(ctx context.Context) (*conn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.histConn != nil {
		return p.histConn, nil
	}
	c, err := dial(ctx, p.cfg.URI, p.cfg.Username, p.cfg.Password, appName, appVersion, p.cfg.SystemName, infraHistPlant)
	if err != nil {
		return nil, err
	}
	p.histConn = c
	return c, nil
}

// ---------------------------------------------------------------------------
// Account + trade route resolution
// ---------------------------------------------------------------------------

func (p *Provider) resolveFirstAccount(ctx context.Context, c *conn) (string, error) {
	// First get login info for fcm/ib IDs
	resp, err := c.sendAndRecv(ctx, buildRequestLoginInfo(), tplResponseLoginInfo)
	if err != nil {
		return "", fmt.Errorf("rithmic login info: %w", err)
	}
	if resp.FcmID != "" {
		p.fcmID = resp.FcmID
	}
	if resp.IbID != "" {
		p.ibID = resp.IbID
	}

	accounts, err := c.sendAndRecvAll(ctx, buildRequestAccountList(p.fcmID, p.ibID), tplResponseAccountList)
	if err != nil {
		return "", fmt.Errorf("rithmic account list: %w", err)
	}
	if len(accounts) == 0 {
		return "", fmt.Errorf("rithmic: no accounts available")
	}

	acct := accounts[0]
	log.Printf("rithmic: auto-selected account %s (%s) fcm=%s ib=%s",
		acct.AccountID, acct.AccountName, acct.FcmID, acct.IbID)

	if acct.FcmID != "" {
		p.fcmID = acct.FcmID
	}
	if acct.IbID != "" {
		p.ibID = acct.IbID
	}
	return acct.AccountID, nil
}

func (p *Provider) resolveTradeRoutes(ctx context.Context, c *conn) error {
	routes, err := c.sendAndRecvAll(ctx, buildRequestTradeRoutes(), tplResponseTradeRoutes)
	if err != nil {
		return err
	}

	p.routesMu.Lock()
	defer p.routesMu.Unlock()
	for _, r := range routes {
		if r.Exchange != "" && r.TradeRoute != "" {
			key := r.Exchange
			// Prefer default routes
			if _, exists := p.routes[key]; !exists || r.IsDefault {
				p.routes[key] = r.TradeRoute
			}
		}
	}

	log.Printf("rithmic: resolved %d trade routes", len(p.routes))
	return nil
}

func (p *Provider) tradeRouteFor(exchange string) string {
	p.routesMu.RLock()
	defer p.routesMu.RUnlock()
	return p.routes[exchange]
}

// ---------------------------------------------------------------------------
// Symbol helpers — users pass "ES", "NQ", etc. We need exchange.
// ---------------------------------------------------------------------------

func (p *Provider) exchange() string {
	return p.cfg.Exchange
}

// ---------------------------------------------------------------------------
// MarketData implementation
// ---------------------------------------------------------------------------

func (p *Provider) FetchBars(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]provider.Bar, error) {
	c, err := p.getHistConn(ctx)
	if err != nil {
		return nil, err
	}

	go c.startHeartbeat(ctx)

	startSsboe := int32(start.Unix())
	finishSsboe := int32(end.Unix())

	req := buildRequestTickBarReplay(symbol, p.exchange(), startSsboe, finishSsboe)
	bars, err := c.sendAndRecvAll(ctx, req, tplResponseTickBarReplay)
	if err != nil {
		return nil, fmt.Errorf("rithmic tick bar replay: %w", err)
	}

	result := make([]provider.Bar, 0, len(bars))
	for _, b := range bars {
		ts := time.Unix(0, 0)
		if len(b.DataBarSsboe) > 0 {
			secs := int64(b.DataBarSsboe[0])
			usecs := int64(0)
			if len(b.DataBarUsecs) > 0 {
				usecs = int64(b.DataBarUsecs[0])
			}
			ts = time.Unix(secs, usecs*1000).UTC()
		}

		result = append(result, provider.Bar{
			Symbol:    symbol,
			Timestamp: ts,
			Open:      b.OpenPrice,
			High:      b.HighPrice,
			Low:       b.LowPrice,
			Close:     b.ClosePrice,
			Volume:    float64(b.BarVolume),
		})
	}

	// Aggregate tick bars into timeframe bars
	if len(result) > 0 {
		result = aggregateBars(result, timeframe)
	}

	return result, nil
}

func (p *Provider) FetchBarsMulti(ctx context.Context, symbols []string, timeframe string, start, end time.Time) ([]provider.Bar, error) {
	var all []provider.Bar
	for _, sym := range symbols {
		bars, err := p.FetchBars(ctx, sym, timeframe, start, end)
		if err != nil {
			return nil, err
		}
		all = append(all, bars...)
	}
	return all, nil
}

func (p *Provider) SubscribeBars(ctx context.Context, symbols []string, timeframe string, handler func(provider.Bar)) error {
	c, err := p.getTickerConn(ctx)
	if err != nil {
		return err
	}

	// Start heartbeat in background
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go c.startHeartbeat(hbCtx)

	// Subscribe to market data for each symbol
	for _, sym := range symbols {
		if err := c.send(ctx, buildRequestMarketData(sym, p.exchange(), true)); err != nil {
			return fmt.Errorf("rithmic subscribe %s: %w", sym, err)
		}
	}
	defer func() {
		for _, sym := range symbols {
			c.send(context.Background(), buildRequestMarketData(sym, p.exchange(), false))
		}
	}()

	// Read messages and aggregate bars
	ch := make(chan []byte, 1024)
	go c.readLoop(ctx, ch)

	interval := barInterval(timeframe)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	type barState struct {
		open, high, low, close float64
		volume                 float64
		hasData                bool
	}
	barStates := make(map[string]*barState)
	for _, sym := range symbols {
		barStates[sym] = &barState{}
	}

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-ticker.C:
			barTime := time.Now().UTC().Truncate(interval)
			for sym, bs := range barStates {
				if !bs.hasData {
					continue
				}
				handler(provider.Bar{
					Symbol:    sym,
					Timestamp: barTime,
					Open:      bs.open,
					High:      bs.high,
					Low:       bs.low,
					Close:     bs.close,
					Volume:    bs.volume,
				})
				barStates[sym] = &barState{}
			}

		case data := <-ch:
			msg, err := decodeMsg(data)
			if err != nil {
				continue
			}

			switch msg.TemplateID {
			case tplResponseMarketData:
				if len(msg.RpCode) > 0 && msg.RpCode[0] != "0" {
					log.Printf("rithmic: market data subscribe error: %v", msg.UserMsg)
				}
			case tplLastTrade:
				if msg.PresenceBits&presenceLastTrade == 0 {
					continue
				}
				sym := msg.Symbol
				bs, ok := barStates[sym]
				if !ok {
					continue
				}
				price := msg.TradePrice
				if !bs.hasData {
					bs.open = price
					bs.high = price
					bs.low = price
					bs.hasData = true
				}
				if price > bs.high {
					bs.high = price
				}
				if price < bs.low {
					bs.low = price
				}
				bs.close = price
				bs.volume += float64(msg.TradeSize)
			}
		}
	}
}

func (p *Provider) SubscribeTrades(ctx context.Context, symbols []string, handler func(provider.Trade)) error {
	c, err := p.getTickerConn(ctx)
	if err != nil {
		return err
	}

	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go c.startHeartbeat(hbCtx)

	for _, sym := range symbols {
		if err := c.send(ctx, buildRequestMarketData(sym, p.exchange(), true)); err != nil {
			return fmt.Errorf("rithmic subscribe %s: %w", sym, err)
		}
	}
	defer func() {
		for _, sym := range symbols {
			c.send(context.Background(), buildRequestMarketData(sym, p.exchange(), false))
		}
	}()

	ch := make(chan []byte, 1024)
	go c.readLoop(ctx, ch)

	for {
		select {
		case <-ctx.Done():
			return nil
		case data := <-ch:
			msg, err := decodeMsg(data)
			if err != nil || msg.TemplateID != tplLastTrade {
				continue
			}
			if msg.PresenceBits&presenceLastTrade == 0 {
				continue
			}
			ts := rithmicTime(msg.Ssboe, msg.Usecs)
			handler(provider.Trade{
				Symbol:    msg.Symbol,
				Timestamp: ts,
				Price:     msg.TradePrice,
				Size:      float64(msg.TradeSize),
			})
		}
	}
}

func (p *Provider) SubscribeQuotes(ctx context.Context, symbols []string, handler func(provider.Quote)) error {
	c, err := p.getTickerConn(ctx)
	if err != nil {
		return err
	}

	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go c.startHeartbeat(hbCtx)

	for _, sym := range symbols {
		if err := c.send(ctx, buildRequestMarketData(sym, p.exchange(), true)); err != nil {
			return fmt.Errorf("rithmic subscribe %s: %w", sym, err)
		}
	}
	defer func() {
		for _, sym := range symbols {
			c.send(context.Background(), buildRequestMarketData(sym, p.exchange(), false))
		}
	}()

	ch := make(chan []byte, 1024)
	go c.readLoop(ctx, ch)

	for {
		select {
		case <-ctx.Done():
			return nil
		case data := <-ch:
			msg, err := decodeMsg(data)
			if err != nil || msg.TemplateID != tplBestBidOffer {
				continue
			}

			p.quotesMu.Lock()
			qs, ok := p.quotes[msg.Symbol]
			if !ok {
				qs = &quoteState{}
				p.quotes[msg.Symbol] = qs
			}
			if msg.PresenceBits&presenceBid != 0 {
				qs.bidPrice = msg.BidPrice
				qs.bidSize = float64(msg.BidSize)
			}
			if msg.PresenceBits&presenceAsk != 0 {
				qs.askPrice = msg.AskPrice
				qs.askSize = float64(msg.AskSize)
			}
			qs.ts = rithmicTime(msg.Ssboe, msg.Usecs)
			p.quotesMu.Unlock()

			handler(provider.Quote{
				Symbol:    msg.Symbol,
				Timestamp: qs.ts,
				BidPrice:  qs.bidPrice,
				BidSize:   qs.bidSize,
				AskPrice:  qs.askPrice,
				AskSize:   qs.askSize,
			})
		}
	}
}

// ---------------------------------------------------------------------------
// Execution implementation
// ---------------------------------------------------------------------------

func (p *Provider) GetAccount(ctx context.Context) (provider.Account, error) {
	// Rithmic doesn't have a simple cash/equity REST endpoint.
	// We ensure order conn is established (which resolved account).
	if _, err := p.getOrderConn(ctx); err != nil {
		return provider.Account{}, err
	}
	// Return a stub — real P&L tracking is done by the engine.
	return provider.Account{Cash: 0, Equity: 0}, nil
}

func (p *Provider) GetPositions(ctx context.Context) ([]provider.Position, error) {
	// Rithmic's position data comes through PNL_PLANT or order notifications.
	// For now return empty — the engine tracks positions from fills.
	if _, err := p.getOrderConn(ctx); err != nil {
		return nil, err
	}
	return nil, nil
}

func (p *Provider) GetOpenOrders(ctx context.Context) ([]provider.OpenOrder, error) {
	// Open orders are tracked via order notification stream.
	// Return empty — the engine uses SubscribeFills for order tracking.
	if _, err := p.getOrderConn(ctx); err != nil {
		return nil, err
	}
	return nil, nil
}

func (p *Provider) PlaceOrder(ctx context.Context, order strategy.Order) (provider.OrderResult, error) {
	c, err := p.getOrderConn(ctx)
	if err != nil {
		return provider.OrderResult{}, err
	}

	exchange := p.exchange()
	route := p.tradeRouteFor(exchange)
	if route == "" {
		return provider.OrderResult{}, fmt.Errorf("rithmic: no trade route for exchange %s", exchange)
	}

	txnType := int32(txnBuy)
	if order.Side == "sell" {
		txnType = txnSell
	}

	var pt int32
	var price, triggerPrice float64
	switch strings.ToLower(order.OrderType) {
	case "limit":
		pt = priceLimit
		price = order.LimitPrice
	case "stop":
		pt = priceStopMarket
		triggerPrice = order.StopPrice
	case "stop_limit":
		pt = priceStopLimit
		price = order.LimitPrice
		triggerPrice = order.StopPrice
	default: // "market" or ""
		pt = priceMarket
	}

	qty := int32(order.Qty)
	if qty <= 0 {
		qty = 1
	}

	userTag := fmt.Sprintf("bb_%d", time.Now().UnixMilli())

	req := buildRequestNewOrder(
		p.fcmID, p.ibID, p.accountID,
		order.Symbol, exchange, route,
		qty, txnType, pt, price, triggerPrice, userTag,
	)

	resp, err := c.sendAndRecv(ctx, req, tplResponseNewOrder)
	if err != nil {
		return provider.OrderResult{}, fmt.Errorf("rithmic new order: %w", err)
	}

	if len(resp.RpCode) > 0 && resp.RpCode[0] != "0" {
		return provider.OrderResult{}, fmt.Errorf("rithmic order rejected: rp_code=%v msg=%v", resp.RpCode, resp.UserMsg)
	}

	basketID := resp.BasketID
	log.Printf("rithmic: order placed basket_id=%s symbol=%s side=%s qty=%d type=%s",
		basketID, order.Symbol, order.Side, qty, order.OrderType)

	// Place bracket orders (SL/TP) as separate orders if requested
	if order.SLDistance > 0 || order.TPDistance > 0 {
		go p.placeBracketOrders(ctx, order, basketID)
	}

	return provider.OrderResult{ID: basketID}, nil
}

func (p *Provider) placeBracketOrders(ctx context.Context, parentOrder strategy.Order, parentBasketID string) {
	// Wait briefly for the parent to fill so we can estimate entry price.
	// Bracket orders on Rithmic are separate orders — we place them
	// based on estimated fill price from current market.
	time.Sleep(500 * time.Millisecond)

	c, err := p.getOrderConn(ctx)
	if err != nil {
		log.Printf("rithmic: bracket order conn error: %v", err)
		return
	}

	// Get approximate entry from quote state
	p.quotesMu.RLock()
	qs := p.quotes[parentOrder.Symbol]
	var entryPrice float64
	if qs != nil {
		if parentOrder.Side == "buy" {
			entryPrice = qs.askPrice
		} else {
			entryPrice = qs.bidPrice
		}
		if entryPrice == 0 {
			entryPrice = qs.lastPrice
		}
	}
	p.quotesMu.RUnlock()

	if entryPrice == 0 {
		log.Printf("rithmic: cannot place brackets — no price for %s", parentOrder.Symbol)
		return
	}

	exchange := p.exchange()
	route := p.tradeRouteFor(exchange)
	exitSide := int32(txnSell)
	if parentOrder.Side == "sell" {
		exitSide = txnBuy
	}
	qty := int32(parentOrder.Qty)
	if qty <= 0 {
		qty = 1
	}

	// Stop loss
	if parentOrder.SLDistance > 0 {
		var slPrice float64
		if parentOrder.Side == "buy" {
			slPrice = entryPrice - parentOrder.SLDistance
		} else {
			slPrice = entryPrice + parentOrder.SLDistance
		}
		tag := fmt.Sprintf("bb_sl_%d", time.Now().UnixMilli())
		req := buildRequestNewOrder(p.fcmID, p.ibID, p.accountID,
			parentOrder.Symbol, exchange, route,
			qty, exitSide, priceStopMarket, 0, slPrice, tag)
		if err := c.send(ctx, req); err != nil {
			log.Printf("rithmic: SL order send error: %v", err)
		} else {
			log.Printf("rithmic: SL order sent at %.2f for %s", slPrice, parentOrder.Symbol)
		}
	}

	// Take profit
	if parentOrder.TPDistance > 0 {
		var tpPrice float64
		if parentOrder.Side == "buy" {
			tpPrice = entryPrice + parentOrder.TPDistance
		} else {
			tpPrice = entryPrice - parentOrder.TPDistance
		}
		tag := fmt.Sprintf("bb_tp_%d", time.Now().UnixMilli())
		req := buildRequestNewOrder(p.fcmID, p.ibID, p.accountID,
			parentOrder.Symbol, exchange, route,
			qty, exitSide, priceLimit, tpPrice, 0, tag)
		if err := c.send(ctx, req); err != nil {
			log.Printf("rithmic: TP order send error: %v", err)
		} else {
			log.Printf("rithmic: TP order sent at %.2f for %s", tpPrice, parentOrder.Symbol)
		}
	}
}

func (p *Provider) CancelOrder(ctx context.Context, orderID string) error {
	// Rithmic's cancel order is not in the sample proto set.
	// The Reference Guide lists it but the proto wasn't provided.
	// For now, log a warning.
	log.Printf("rithmic: CancelOrder not yet implemented (order %s)", orderID)
	return fmt.Errorf("rithmic: cancel order not available in current proto set")
}

func (p *Provider) SubscribeFills(ctx context.Context, handler func(provider.Fill)) error {
	c, err := p.getOrderConn(ctx)
	if err != nil {
		return err
	}

	// Start heartbeat
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go c.startHeartbeat(hbCtx)

	// Subscribe for order updates
	req := buildRequestSubscribeOrderUpdates(p.fcmID, p.ibID, p.accountID)
	if err := c.send(ctx, req); err != nil {
		return fmt.Errorf("rithmic subscribe order updates: %w", err)
	}

	ch := make(chan []byte, 512)
	go c.readLoop(ctx, ch)

	for {
		select {
		case <-ctx.Done():
			return nil
		case data := <-ch:
			msg, err := decodeMsg(data)
			if err != nil {
				continue
			}

			switch msg.TemplateID {
			case tplResponseOrderUpdates:
				if len(msg.RpCode) > 0 && msg.RpCode[0] != "0" {
					log.Printf("rithmic: order updates subscribe error: %v", msg.UserMsg)
				}
			case tplExchangeOrderNotification:
				if msg.NotifyType != exchNotifyFill {
					continue
				}
				if msg.FillSize == 0 {
					continue
				}

				side := "buy"
				if msg.TransactionType == txnSell {
					side = "sell"
				}

				ts := rithmicTime(msg.Ssboe, msg.Usecs)
				partial := msg.TotalUnfilledSize > 0

				handler(provider.Fill{
					OrderID:   msg.BasketID,
					Symbol:    msg.Symbol,
					Side:      side,
					Qty:       float64(msg.FillSize),
					Price:     msg.FillPrice,
					Timestamp: ts,
					Partial:   partial,
				})

			case tplRithmicOrderNotification:
				// Log status changes for debugging
				if msg.NotifyType == ronComplete {
					log.Printf("rithmic: order complete basket=%s reason=%s",
						msg.BasketID, msg.CompletionReason)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// SessionNotifier — same CME schedule as TopStepX
// ---------------------------------------------------------------------------

const closeHour, closeMin = 16, 10

func (p *Provider) SubscribeSession(ctx context.Context, handler func(provider.SessionEvent)) error {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return err
	}

	now := time.Now().In(loc)
	if isMarketOpen(now) {
		handler(provider.SessionEvent{Type: "market_open", Time: now})
	} else {
		handler(provider.SessionEvent{Type: "market_close", Time: now})
	}

	for {
		now = time.Now().In(loc)
		nextEvent, eventType := nextSessionEvent(now)
		delay := time.Until(nextEvent)

		log.Printf("rithmic: next session event: %s at %s (in %s)",
			eventType, nextEvent.Format("Mon 2006-01-02 15:04 MST"), delay.Round(time.Second))

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
			handler(provider.SessionEvent{Type: eventType, Time: nextEvent})
		}
	}
}

func isMarketOpen(now time.Time) bool {
	wd := now.Weekday()
	hhmm := now.Hour()*60 + now.Minute()
	closeAt := closeHour*60 + closeMin

	switch wd {
	case time.Saturday:
		return false
	case time.Sunday:
		return hhmm >= 18*60
	case time.Friday:
		return hhmm < closeAt
	default:
		return hhmm < closeAt || hhmm >= 18*60
	}
}

func nextSessionEvent(now time.Time) (time.Time, string) {
	wd := now.Weekday()
	hhmm := now.Hour()*60 + now.Minute()
	closeAt := closeHour*60 + closeMin
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	closeTime := time.Date(day.Year(), day.Month(), day.Day(), closeHour, closeMin, 0, 0, now.Location())
	openTime := time.Date(day.Year(), day.Month(), day.Day(), 18, 0, 0, 0, now.Location())

	switch wd {
	case time.Saturday:
		sunday := day.AddDate(0, 0, 1)
		return time.Date(sunday.Year(), sunday.Month(), sunday.Day(), 18, 0, 0, 0, now.Location()), "market_open"
	case time.Sunday:
		if hhmm < 18*60 {
			return openTime, "market_open"
		}
		monday := day.AddDate(0, 0, 1)
		return time.Date(monday.Year(), monday.Month(), monday.Day(), closeHour, closeMin, 0, 0, now.Location()), "market_close"
	case time.Friday:
		if hhmm < closeAt {
			return closeTime, "market_close"
		}
		sunday := day.AddDate(0, 0, 2)
		return time.Date(sunday.Year(), sunday.Month(), sunday.Day(), 18, 0, 0, 0, now.Location()), "market_open"
	default:
		if hhmm < closeAt {
			return closeTime, "market_close"
		}
		if hhmm < 18*60 {
			return openTime, "market_open"
		}
		tomorrow := day.AddDate(0, 0, 1)
		return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), closeHour, closeMin, 0, 0, now.Location()), "market_close"
	}
}

// ---------------------------------------------------------------------------
// ContractSpecProvider — common futures specs
// ---------------------------------------------------------------------------

var knownSpecs = map[string]provider.ContractSpec{
	"ES":  {Symbol: "ES", TickSize: 0.25, TickValue: 12.50, PointValue: 50.0},
	"NQ":  {Symbol: "NQ", TickSize: 0.25, TickValue: 5.00, PointValue: 20.0},
	"MES": {Symbol: "MES", TickSize: 0.25, TickValue: 1.25, PointValue: 5.0},
	"MNQ": {Symbol: "MNQ", TickSize: 0.25, TickValue: 0.50, PointValue: 2.0},
	"YM":  {Symbol: "YM", TickSize: 1.0, TickValue: 5.00, PointValue: 5.0},
	"MYM": {Symbol: "MYM", TickSize: 1.0, TickValue: 0.50, PointValue: 0.5},
	"RTY": {Symbol: "RTY", TickSize: 0.10, TickValue: 5.00, PointValue: 50.0},
	"M2K": {Symbol: "M2K", TickSize: 0.10, TickValue: 0.50, PointValue: 5.0},
	"CL":  {Symbol: "CL", TickSize: 0.01, TickValue: 10.00, PointValue: 1000.0},
	"GC":  {Symbol: "GC", TickSize: 0.10, TickValue: 10.00, PointValue: 100.0},
}

func (p *Provider) GetContractSpec(ctx context.Context, symbol string) (provider.ContractSpec, error) {
	// Strip any month/year suffix to match base symbol
	base := strings.ToUpper(symbol)
	for k, v := range knownSpecs {
		if strings.HasPrefix(base, k) {
			v.Symbol = symbol
			return v, nil
		}
	}
	return provider.ContractSpec{Symbol: symbol, TickSize: 0.25, TickValue: 12.50, PointValue: 50.0},
		fmt.Errorf("rithmic: unknown contract spec for %s, using ES defaults", symbol)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func rithmicTime(ssboe, usecs int32) time.Time {
	return time.Unix(int64(ssboe), int64(usecs)*1000).UTC()
}

func barInterval(timeframe string) time.Duration {
	tf := strings.ToLower(timeframe)
	switch tf {
	case "1s":
		return time.Second
	case "5s":
		return 5 * time.Second
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
		// Try to parse as duration
		d, err := time.ParseDuration(tf)
		if err == nil {
			return d
		}
		// Try numeric minutes
		if n, err := strconv.Atoi(strings.TrimSuffix(tf, "m")); err == nil {
			return time.Duration(n) * time.Minute
		}
		return time.Minute
	}
}

func aggregateBars(tickBars []provider.Bar, timeframe string) []provider.Bar {
	interval := barInterval(timeframe)
	if interval <= 0 {
		return tickBars
	}

	type agg struct {
		open, high, low, close float64
		volume                 float64
		ts                     time.Time
	}

	buckets := make(map[int64]*agg)
	var keys []int64

	for _, b := range tickBars {
		key := b.Timestamp.Truncate(interval).Unix()
		a, ok := buckets[key]
		if !ok {
			a = &agg{
				open: b.Open,
				high: b.High,
				low:  b.Low,
				ts:   b.Timestamp.Truncate(interval),
			}
			buckets[key] = a
			keys = append(keys, key)
		}
		if b.High > a.high {
			a.high = b.High
		}
		if b.Low < a.low || a.low == 0 {
			a.low = b.Low
		}
		a.close = b.Close
		a.volume += b.Volume
	}

	result := make([]provider.Bar, 0, len(keys))
	for _, k := range keys {
		a := buckets[k]
		result = append(result, provider.Bar{
			Symbol:    tickBars[0].Symbol,
			Timestamp: a.ts,
			Open:      a.open,
			High:      a.high,
			Low:       a.low,
			Close:     a.close,
			Volume:    math.Round(a.volume),
		})
	}
	return result
}

// Compile-time interface checks.
var (
	_ provider.MarketData          = (*Provider)(nil)
	_ provider.Execution           = (*Provider)(nil)
	_ provider.SessionNotifier     = (*Provider)(nil)
	_ provider.ContractSpecProvider = (*Provider)(nil)
)
