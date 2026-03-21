// Package ibkr implements provider.MarketData and provider.Execution using the
// IB Gateway TWS API (socket-based).
//
// Prerequisites:
//   - Run IB Gateway and log in with your paper (or live) account.
//   - In IB Gateway: Edit → Global Configuration → API → Settings
//     ✓ Enable ActiveX and Socket Clients
//     ✗ Read-Only API  (uncheck this)
//     Note the port (default paper: 4002, live: 4001).
//   - Set IBKR_ACCOUNT_ID environment variable (e.g. DUP500161).
//   - Optionally set IBKR_HOST (default 127.0.0.1) and IBKR_PORT (default 4002).
package ibkr

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hadrianl/ibapi"

	"github.com/benny-conn/brandon-bot/provider"
	"github.com/benny-conn/brandon-bot/strategy"
)

const (
	defaultHost = "127.0.0.1"
	defaultPort = 4002
)

// Config holds IB Gateway connection settings.
type Config struct {
	AccountID string `json:"account_id"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
}

// Provider implements provider.MarketData and provider.Execution via IB Gateway TWS API.
type Provider struct {
	mu        sync.Mutex
	tws       *tws
	host      string
	port      int
	accountID string
}

// New creates an IBKR provider. Config fields override env vars where set.
func New(cfg Config) *Provider {
	host := cfg.Host
	if host == "" {
		host = os.Getenv("IBKR_HOST")
	}
	if host == "" {
		host = defaultHost
	}
	port := cfg.Port
	if port == 0 {
		if p := os.Getenv("IBKR_PORT"); p != "" {
			port, _ = strconv.Atoi(p)
		}
	}
	if port == 0 {
		port = defaultPort
	}
	accountID := cfg.AccountID
	if accountID == "" {
		accountID = os.Getenv("IBKR_ACCOUNT_ID")
	}
	return &Provider{
		host:      host,
		port:      port,
		accountID: accountID,
	}
}

// conn returns the shared tws client, creating it on first call.
func (p *Provider) conn() (*tws, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tws != nil {
		return p.tws, nil
	}
	t, err := newTWS(p.host, p.port)
	if err != nil {
		return nil, err
	}
	p.tws = t
	return t, nil
}

// — MarketData —

func (p *Provider) FetchBars(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]provider.Bar, error) {
	t, err := p.conn()
	if err != nil {
		return nil, err
	}

	reqID := t.nextReqID()
	barsCh := make(chan provider.Bar, 2000)
	done := make(chan struct{})

	t.mu.Lock()
	t.histHandlers[reqID] = func(bar provider.Bar) {
		bar.Symbol = symbol
		if (start.IsZero() || !bar.Timestamp.Before(start)) &&
			(end.IsZero() || !bar.Timestamp.After(end)) {
			select {
			case barsCh <- bar:
			default:
			}
		}
	}
	t.histDone[reqID] = done
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		delete(t.histHandlers, reqID)
		delete(t.histDone, reqID)
		t.mu.Unlock()
	}()

	endStr := ""
	if !end.IsZero() {
		endStr = end.UTC().Format("20060102 15:04:05") + " UTC"
	}

	t.ic.ReqHistoricalData(reqID, stockContract(symbol), endStr,
		dateToDuration(start, end), twsBarSize(timeframe),
		"TRADES", true, 2, false, nil)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-done:
	}

	close(barsCh)
	var bars []provider.Bar
	for b := range barsCh {
		bars = append(bars, b)
	}
	sort.Slice(bars, func(i, j int) bool {
		return bars[i].Timestamp.Before(bars[j].Timestamp)
	})
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

// SubscribeBars streams live completed bars using reqHistoricalData with keepUpToDate=true.
// Initial backfill bars are discarded; new bars arrive via HistoricalDataUpdate as each bar closes.
func (p *Provider) SubscribeBars(ctx context.Context, symbols []string, timeframe string, handler func(provider.Bar)) error {
	t, err := p.conn()
	if err != nil {
		return err
	}

	for _, sym := range symbols {
		reqID := t.nextReqID()
		s := sym

		t.mu.Lock()
		t.histHandlers[reqID] = func(provider.Bar) {} // discard backfill
		t.liveBars[reqID] = func(bar provider.Bar) {
			bar.Symbol = s
			handler(bar)
		}
		t.mu.Unlock()

		// keepUpToDate=true: backfill via HistoricalData (discarded), live bars via HistoricalDataUpdate.
		t.ic.ReqHistoricalData(reqID, stockContract(s), "", "1 D",
			twsBarSize(timeframe), "TRADES", true, 2, true, nil)
		log.Printf("ibkr: subscribed to bars for %s (timeframe=%s, reqID=%d)", s, timeframe, reqID)
	}

	<-ctx.Done()
	return nil
}

// SubscribeTrades streams real-time tick-by-tick last-sale prints.
func (p *Provider) SubscribeTrades(ctx context.Context, symbols []string, handler func(provider.Trade)) error {
	t, err := p.conn()
	if err != nil {
		return err
	}

	for _, sym := range symbols {
		reqID := t.nextReqID()
		s := sym

		t.mu.Lock()
		t.tradeHandlers[reqID] = func(trade provider.Trade) {
			trade.Symbol = s
			handler(trade)
		}
		t.mu.Unlock()

		// numberOfTicks=0 means stream indefinitely; ignoreSize=true skips size-only updates.
		t.ic.ReqTickByTickData(reqID, stockContract(s), "Last", 0, true)
		log.Printf("ibkr: subscribed to trades for %s (reqID=%d)", s, reqID)
	}

	<-ctx.Done()
	return nil
}

// — Execution —

func (p *Provider) GetAccount(ctx context.Context) (provider.Account, error) {
	t, err := p.conn()
	if err != nil {
		return provider.Account{}, err
	}

	reqID := t.nextReqID()
	fieldCh := make(chan acctField, 20)
	endCh := make(chan struct{})

	t.mu.Lock()
	t.acctSumm[reqID] = fieldCh
	t.acctEnd[reqID] = endCh
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		delete(t.acctSumm, reqID)
		delete(t.acctEnd, reqID)
		t.mu.Unlock()
	}()

	t.ic.ReqAccountSummary(reqID, "All", "TotalCashValue,NetLiquidation")

	var acct provider.Account
	for {
		select {
		case <-ctx.Done():
			return provider.Account{}, ctx.Err()
		case f := <-fieldCh:
			v, _ := strconv.ParseFloat(f.value, 64)
			switch f.tag {
			case "TotalCashValue":
				acct.Cash = v
			case "NetLiquidation":
				acct.Equity = v
			}
		case <-endCh:
			return acct, nil
		}
	}
}

func (p *Provider) GetPositions(ctx context.Context) ([]provider.Position, error) {
	t, err := p.conn()
	if err != nil {
		return nil, err
	}

	posCh := make(chan posResult, 100)
	posEnd := make(chan struct{})

	t.posMu.Lock()
	t.posCh = posCh
	t.posEnd = posEnd
	t.posMu.Unlock()

	defer func() {
		t.posMu.Lock()
		t.posCh = nil
		t.posEnd = nil
		t.posMu.Unlock()
	}()

	t.ic.ReqPositions()

	var positions []provider.Position
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case r := <-posCh:
			if r.qty == 0 {
				continue
			}
			positions = append(positions, provider.Position{
				Symbol:        r.symbol,
				Qty:           r.qty,
				AvgEntryPrice: r.avg,
			})
		case <-posEnd:
			return positions, nil
		}
	}
}

// PlaceOrder submits an order to IB Gateway. It returns immediately with the
// order ID; the actual fill arrives asynchronously via SubscribeFills.
func (p *Provider) PlaceOrder(ctx context.Context, order strategy.Order) (provider.OrderResult, error) {
	t, err := p.conn()
	if err != nil {
		return provider.OrderResult{}, err
	}

	orderID := t.nextOrderID()

	ibOrder := &ibapi.Order{
		OrderID:       orderID,
		Action:        strings.ToUpper(order.Side),
		TotalQuantity: order.Qty,
		OrderType:     "MKT",
		TIF:           "DAY",
		Transmit:      true,
	}
	if order.OrderType == "limit" && order.LimitPrice > 0 {
		ibOrder.OrderType = "LMT"
		ibOrder.LimitPrice = order.LimitPrice
		ibOrder.TIF = "GTC"
	}

	t.ic.PlaceOrder(orderID, stockContract(order.Symbol), ibOrder)
	log.Printf("ibkr: placed %s %s x%.0f (orderID=%d)", ibOrder.Action, order.Symbol, order.Qty, orderID)
	return provider.OrderResult{ID: fmt.Sprintf("%d", orderID)}, nil
}

// SubscribeFills registers the fill handler and requests any executions from
// today to catch fills that occurred before startup.
func (p *Provider) SubscribeFills(ctx context.Context, handler func(provider.Fill)) error {
	t, err := p.conn()
	if err != nil {
		return err
	}

	t.mu.Lock()
	t.fillHandler = handler
	t.mu.Unlock()

	// Replay any fills from today at startup.
	t.ic.ReqExecutions(t.nextReqID(), ibapi.ExecutionFilter{})

	<-ctx.Done()

	t.mu.Lock()
	t.fillHandler = nil
	t.mu.Unlock()

	return nil
}
