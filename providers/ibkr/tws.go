package ibkr

import (
	"fmt"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hadrianl/ibapi"

	"github.com/benny-conn/brandon-bot/provider"
)

// — internal channel types —

type acctField struct{ tag, value string }

type posResult struct {
	symbol string
	qty    float64
	avg    float64
}

// tws wraps hadrianl/ibapi and bridges its callbacks to channels/handlers.
// It embeds ibapi.Wrapper for default (zap-logged) no-op implementations of
// all 89 IbWrapper methods; we only override the ones we actually use.
type tws struct {
	ibapi.Wrapper // default no-op implementations for unused callbacks

	ic    *ibapi.IbClient
	ready chan struct{} // closed when NextValidID fires (connection is ready)

	reqSeq atomic.Int64
	ordSeq atomic.Int64

	mu            sync.RWMutex
	histHandlers  map[int64]func(provider.Bar) // reqID → backfill bar handler
	liveBars      map[int64]func(provider.Bar) // reqID → HistoricalDataUpdate handler
	histDone      map[int64]chan struct{}       // reqID → signals backfill complete
	tradeHandlers map[int64]func(provider.Trade)
	acctSumm      map[int64]chan acctField
	acctEnd       map[int64]chan struct{}
	fillHandler   func(provider.Fill)
	orderChans    map[int64]chan string // orderID → status channel

	posMu  sync.Mutex
	posCh  chan posResult
	posEnd chan struct{}
}

func newTWS(host string, port int) (*tws, error) {
	t := &tws{
		ready:         make(chan struct{}),
		histHandlers:  make(map[int64]func(provider.Bar)),
		liveBars:      make(map[int64]func(provider.Bar)),
		histDone:      make(map[int64]chan struct{}),
		tradeHandlers: make(map[int64]func(provider.Trade)),
		acctSumm:      make(map[int64]chan acctField),
		acctEnd:       make(map[int64]chan struct{}),
		orderChans:    make(map[int64]chan string),
	}

	ic := ibapi.NewIbClient(t)
	if err := ic.Connect(host, port, 1); err != nil {
		return nil, fmt.Errorf("TWS connect: %w", err)
	}
	if err := ic.HandShake(); err != nil {
		return nil, fmt.Errorf("TWS handshake: %w", err)
	}
	t.ic = ic
	go func() { _ = ic.Run() }()

	select {
	case <-t.ready:
		return t, nil
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("TWS: timed out waiting for NextValidID (is IB Gateway running and API enabled?)")
	}
}

func (t *tws) nextReqID() int64   { return t.reqSeq.Add(1) }
func (t *tws) nextOrderID() int64 { return t.ordSeq.Add(1) }

// — IbWrapper overrides (only the callbacks we use) —

func (t *tws) NextValidID(reqID int64) {
	t.ordSeq.Store(reqID)
	select {
	case <-t.ready:
	default:
		close(t.ready)
	}
	log.Printf("ibkr: connected, next order ID = %d", reqID)
}

func (t *tws) HistoricalData(reqID int64, bar *ibapi.BarData) {
	t.mu.RLock()
	h := t.histHandlers[reqID]
	t.mu.RUnlock()
	if h != nil {
		if b, ok := parseBarData(bar); ok {
			h(b)
		}
	}
}

func (t *tws) HistoricalDataEnd(reqID int64, _, _ string) {
	t.mu.RLock()
	ch := t.histDone[reqID]
	t.mu.RUnlock()
	if ch != nil {
		select {
		case <-ch: // already closed
		default:
			close(ch)
		}
	}
}

func (t *tws) HistoricalDataUpdate(reqID int64, bar *ibapi.BarData) {
	t.mu.RLock()
	h := t.liveBars[reqID]
	t.mu.RUnlock()
	if h != nil {
		if b, ok := parseBarData(bar); ok {
			h(b)
		}
	}
}

func (t *tws) RealtimeBar(reqID int64, time_ int64, open, high, low, close_ float64, volume int64, _ float64, _ int64) {
	t.mu.RLock()
	h := t.liveBars[reqID]
	t.mu.RUnlock()
	if h != nil {
		h(provider.Bar{
			Timestamp: time.Unix(time_, 0).UTC(),
			Open:      open,
			High:      high,
			Low:       low,
			Close:     close_,
			Volume:    float64(volume),
		})
	}
}

func (t *tws) TickByTickAllLast(reqID int64, _ int64, time_ int64, price float64, size int64, _ ibapi.TickAttribLast, _ string, _ string) {
	t.mu.RLock()
	h := t.tradeHandlers[reqID]
	t.mu.RUnlock()
	if h != nil {
		h(provider.Trade{
			Timestamp: time.Unix(time_, 0).UTC(),
			Price:     price,
			Size:      float64(size),
		})
	}
}

func (t *tws) AccountSummary(reqID int64, _, tag, value, _ string) {
	t.mu.RLock()
	ch := t.acctSumm[reqID]
	t.mu.RUnlock()
	if ch != nil {
		select {
		case ch <- acctField{tag: tag, value: value}:
		default:
		}
	}
}

func (t *tws) AccountSummaryEnd(reqID int64) {
	t.mu.RLock()
	ch := t.acctEnd[reqID]
	t.mu.RUnlock()
	if ch != nil {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
}

func (t *tws) Position(_ string, contract *ibapi.Contract, position, avgCost float64) {
	t.posMu.Lock()
	ch := t.posCh
	t.posMu.Unlock()
	if ch != nil {
		ch <- posResult{symbol: contract.Symbol, qty: position, avg: avgCost}
	}
}

func (t *tws) PositionEnd() {
	t.posMu.Lock()
	ch := t.posEnd
	t.posMu.Unlock()
	if ch != nil {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
}

func (t *tws) OrderStatus(orderID int64, status string, _ float64, _ float64, _ float64, _ int64, _ int64, _ float64, _ int64, _ string, _ float64) {
	t.mu.RLock()
	ch := t.orderChans[orderID]
	t.mu.RUnlock()
	if ch != nil {
		select {
		case ch <- status:
		default:
		}
	}
}

func (t *tws) ExecDetails(_ int64, contract *ibapi.Contract, exec *ibapi.Execution) {
	t.mu.RLock()
	h := t.fillHandler
	t.mu.RUnlock()
	if h == nil {
		return
	}
	side := "buy"
	if exec.Side == "SLD" {
		side = "sell"
	}
	h(provider.Fill{
		OrderID:   strconv.FormatInt(exec.OrderID, 10),
		Symbol:    contract.Symbol,
		Side:      side,
		Qty:       exec.Shares,
		Price:     exec.Price,
		Timestamp: time.Now().UTC(),
	})
}

func (t *tws) Error(reqID int64, errCode int64, errString string) {
	if errCode >= 2000 {
		log.Printf("ibkr info [%d]: %s", errCode, errString)
	} else {
		log.Printf("ibkr error [req=%d code=%d]: %s", reqID, errCode, errString)
	}
}

// — helpers —

func parseBarData(bar *ibapi.BarData) (provider.Bar, bool) {
	// TWS sends Date as Unix epoch string when formatDate=2.
	epoch, err := strconv.ParseInt(bar.Date, 10, 64)
	var ts time.Time
	if err == nil {
		ts = time.Unix(epoch, 0).UTC()
	} else {
		// Fallback: "YYYYMMDD  HH:MM:SS"
		ts, err = time.ParseInLocation("20060102  15:04:05", bar.Date, time.UTC)
		if err != nil {
			return provider.Bar{}, false
		}
	}
	return provider.Bar{
		Timestamp: ts,
		Open:      bar.Open,
		High:      bar.High,
		Low:       bar.Low,
		Close:     bar.Close,
		Volume:    bar.Volume,
	}, true
}

func stockContract(symbol string) *ibapi.Contract {
	return &ibapi.Contract{
		Symbol:       symbol,
		SecurityType: "STK",
		Exchange:     "SMART",
		Currency:     "USD",
	}
}

func twsBarSize(timeframe string) string {
	switch timeframe {
	case "1s":
		return "1 secs"
	case "5m":
		return "5 mins"
	case "15m":
		return "15 mins"
	case "1h":
		return "1 hour"
	case "1d":
		return "1 day"
	default:
		return "1 min"
	}
}

func dateToDuration(start, end time.Time) string {
	if end.IsZero() {
		end = time.Now()
	}
	if start.IsZero() {
		return "1 D"
	}
	days := int(end.Sub(start).Hours()/24) + 1
	switch {
	case days <= 1:
		return "1 D"
	case days <= 365:
		return fmt.Sprintf("%d D", days)
	default:
		return fmt.Sprintf("%d Y", (days+364)/365)
	}
}
