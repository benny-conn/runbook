package engine

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/benny-conn/runbook/internal/barbuf"
	"github.com/benny-conn/runbook/internal/bracket"
	"github.com/benny-conn/runbook/internal/orderutil"
	"github.com/benny-conn/runbook/internal/portfolio"
	"github.com/benny-conn/runbook/provider"
	"github.com/benny-conn/runbook/strategy"
)

// Store is the persistence interface used by the engine to log orders, fills,
// and portfolio snapshots. The engine doesn't care whether the underlying
// implementation targets SQLite, Postgres, or anything else — callers provide
// their own implementation.
type Store interface {
	LogOrder(order strategy.Order, brokerOrderID string) error
	LogFill(fill strategy.Fill) error
	LogSnapshot(cash, equity, totalPL float64) error
}

// Logger is an optional interface consumers can implement to redirect engine
// log output. When nil in Config, the standard library log package is used.
type Logger interface {
	Printf(format string, v ...any)
}

// defaultLogger wraps the standard log package.
type defaultLogger struct{}

func (defaultLogger) Printf(format string, v ...any) { log.Printf(format, v...) }

// DefaultLogger returns a Logger that writes to the standard library log package.
func DefaultLogger() Logger { return defaultLogger{} }

// Snapshot is a point-in-time view of engine state. Returned by Engine.Snapshot()
// and passed to Config.OnStatus callbacks.
type Snapshot struct {
	Uptime         time.Duration
	BarsReceived   int64
	TradesReceived int64
	QuotesReceived int64
	FillsReceived  int64
	OrdersPlaced   int64
	OrdersErrored  int64
	Portfolio      PortfolioSnapshot
	Symbols        map[string]SymbolStatus
	RuntimeErrors  []string
}

// PortfolioSnapshot captures portfolio state at a point in time.
type PortfolioSnapshot struct {
	Cash      float64
	Equity    float64
	TotalPL   float64
	DailyPL   float64
	Positions []strategy.Position
}

// SymbolStatus tracks per-symbol data freshness.
type SymbolStatus struct {
	LastBarTime time.Time
}

// MarketSchedule defines when the market opens and closes for clock-based
// session events. Used as a fallback when the provider doesn't implement
// SessionNotifier.
type MarketSchedule struct {
	Open     string // "09:30" (24h format, HH:MM)
	Close    string // "16:00"
	Timezone string // IANA timezone, e.g. "America/New_York"
}

// NYSESchedule returns the default NYSE/NASDAQ market schedule.
func NYSESchedule() *MarketSchedule {
	return &MarketSchedule{
		Open:     "09:30",
		Close:    "16:00",
		Timezone: "America/New_York",
	}
}

// Config holds engine configuration.
type Config struct {
	Capital        float64
	WarmupBars     int             // number of historical bars to replay on startup for indicator warm-up
	MaxWarmupBars  int             // cap on warmup bars when using WarmupFrom (default 300)
	WarmupFrom     time.Time       // if set, warm up from this time (e.g. strategy creation date); 0 = use WarmupBars only
	ConfigJSON     []byte          // raw JSON config passed to Initializer.OnInit (nil if none)
	MarketSchedule *MarketSchedule // market hours for DailySessionHandler; nil defaults to NYSE

	// Positions overrides the broker's GetPositions() response during recovery.
	// When set (non-nil, even if empty), the engine uses these positions instead
	// of querying the broker. This allows the backend to supply per-strategy
	// position state so each strategy resumes exactly where it left off — even
	// when multiple strategies share the same broker account.
	//
	// The Cash field is still read from the broker (GetAccount) unless
	// CashOverride is set.
	Positions []provider.Position

	// CashOverride, when > 0, overrides the broker's GetAccount().Cash during
	// recovery. Use together with Positions for fully backend-controlled recovery.
	CashOverride float64

	// RealizedPL seeds the portfolio's accumulated realized P&L on recovery.
	// When an engine restarts, the P&L counter would otherwise reset to 0.
	// The backend computes this from persisted fills so the live P&L display
	// stays accurate across restarts.
	RealizedPL float64

	// MaxContracts caps the qty on any single order (0 = no limit).
	MaxContracts int

	// FlattenAtClose auto-flattens all positions at market close if the strategy
	// doesn't fully close them itself.
	FlattenAtClose bool

	// Logger, when set, receives all engine log output. If nil, the standard
	// library log package is used. Consumers can implement this to redirect
	// logs to slog, zap, a file, or any other destination.
	Logger Logger

	// StatusInterval controls how often the engine logs periodic status updates.
	// Default (zero value) is 60 seconds. Set to a negative duration to disable
	// status logging entirely.
	StatusInterval time.Duration

	// OnStatus, when set, is called on each status tick with a structured
	// snapshot of the engine's state. Use this to build dashboards, forward
	// metrics to monitoring systems, or implement custom alerting.
	OnStatus func(Snapshot)
}

func DefaultConfig(capital float64) Config {
	return Config{
		Capital:       capital,
		WarmupBars:    100,
		MaxWarmupBars: 300,
	}
}

// engineEvent is a sum type that serializes all incoming events into a single
// channel so all strategy calls happen on one goroutine — strategies need not
// be thread-safe.
type engineEvent interface{ isEvent() }

type tickEvent struct{ tick strategy.Tick }
type tradeEvent struct{ trade strategy.Trade }
type fillEvent struct{ fill strategy.Fill }
type quoteEvent struct{ quote strategy.Quote }
type marketOpenEvent struct{}
type marketCloseEvent struct{}

func (tickEvent) isEvent()        {}
func (tradeEvent) isEvent()       {}
func (fillEvent) isEvent()        {}
func (quoteEvent) isEvent()       {}
func (marketOpenEvent) isEvent()  {}
func (marketCloseEvent) isEvent() {}

// pendingStop is a stop or stop-limit order held locally by the engine when
// the provider doesn't support native stop orders (implements ClientSideStops).
type pendingStop struct {
	order strategy.Order
}


// Engine is the paper trading engine. It streams live data from the provider,
// calls the strategy on each event, submits returned orders, and processes
// async fill events — all serialized through a single event loop.
type Engine struct {
	strategy  strategy.Strategy
	portfolio *portfolio.SimulatedPortfolio
	md        provider.MarketData
	exec      provider.Execution
	store     Store
	config    Config
	logger    Logger
	eventCh   chan engineEvent
	fillCh    chan fillEvent // separate unbuffered channel — fills are never dropped
	ctx       context.Context // set in Run, used by submitOrders

	// Client-side stop order management for providers that don't support native stops.
	clientSideStops bool
	pendingStops    []pendingStop

	// baseTimeframe is the finest timeframe from the strategy's Timeframes(),
	// used for provider subscriptions and warmup. Set at the start of Run().
	baseTimeframe string

	// Multi-timeframe bar aggregation. Non-nil when the strategy implements
	// strategy declares more than one timeframe.
	aggregators []*BarAggregator

	// warmingUp is true during recovery replay — orders are simulated locally
	// instead of being sent to the broker.
	warmingUp bool

	// warmupBrackets tracks TP/SL brackets during warmup for simulation.
	warmupBrackets []bracket.Pending

	// contractSpecs caches per-symbol contract specs queried during Run().
	contractSpecs map[string]provider.ContractSpec

	// barBuffer stores the last N bars per symbol for the bars() global.
	barBuffer *barbuf.Buffer

	// dailyTracker tracks daily high/low/open/close per symbol.
	dailyTracker *barbuf.DailyTracker

	// Observability counters for periodic status logging.
	stats engineStats
}

// engineStats tracks event counters for periodic status logging.
type engineStats struct {
	barsReceived   int64
	tradesReceived int64
	quotesReceived int64
	fillsReceived  int64
	ordersPlaced   int64
	ordersErrored  int64
	lastBarTime    map[string]time.Time // symbol → last bar timestamp
	lastBarAt      time.Time            // wall-clock time of last bar received
	startTime      time.Time
}

// NewEngine constructs a paper trading engine. md and exec may be the same
// object (e.g. *alpaca.Provider) or separate implementations.
func NewEngine(strat strategy.Strategy, md provider.MarketData, exec provider.Execution, store Store, cfg Config) *Engine {
	_, clientStops := exec.(provider.ClientSideStops)
	l := cfg.Logger
	if l == nil {
		l = defaultLogger{}
	}
	return &Engine{
		strategy:        strat,
		portfolio:       portfolio.NewSimulatedPortfolio(cfg.Capital),
		md:              md,
		exec:            exec,
		store:           store,
		config:          cfg,
		logger:          l,
		eventCh:         make(chan engineEvent, 512),
		fillCh:          make(chan fillEvent, 64), // buffered but never dropped
		clientSideStops: clientStops,
		barBuffer:       barbuf.New(),
		dailyTracker:    barbuf.NewDailyTracker(),
	}
}

// logf logs a formatted message through the configured logger.
func (e *Engine) logf(format string, v ...any) {
	e.logger.Printf(format, v...)
}

// Snapshot returns a point-in-time view of the engine's state. Safe to call
// from any goroutine while the engine is running.
func (e *Engine) Snapshot() Snapshot {
	s := &e.stats
	snap := Snapshot{
		Uptime:         time.Since(s.startTime),
		BarsReceived:   s.barsReceived,
		TradesReceived: s.tradesReceived,
		QuotesReceived: s.quotesReceived,
		FillsReceived:  s.fillsReceived,
		OrdersPlaced:   s.ordersPlaced,
		OrdersErrored:  s.ordersErrored,
		Symbols:        make(map[string]SymbolStatus, len(s.lastBarTime)),
	}

	for sym, t := range s.lastBarTime {
		snap.Symbols[sym] = SymbolStatus{LastBarTime: t}
	}

	positions := e.portfolio.Positions()
	snap.Portfolio = PortfolioSnapshot{
		Cash:      e.portfolio.Cash(),
		Equity:    e.portfolio.Equity(),
		TotalPL:   e.portfolio.TotalPL(),
		DailyPL:   e.portfolio.DailyPL(),
		Positions: positions,
	}

	if reporter, ok := e.strategy.(strategy.RuntimeErrorReporter); ok {
		snap.RuntimeErrors = reporter.RuntimeErrors()
	}

	return snap
}

// Run starts the paper trading engine. It blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context, symbols []string) error {
	e.ctx = ctx

	// Check if providers support asset search or legacy market discovery.
	var search strategy.AssetSearch
	if s, ok := e.md.(strategy.AssetSearch); ok {
		search = s
	} else if s, ok := e.exec.(strategy.AssetSearch); ok {
		search = s
	}
	if search != nil {
		e.logf("paper engine: provider supports asset search")
	}

	var discovery strategy.MarketDiscovery
	if md, ok := e.md.(strategy.MarketDiscovery); ok {
		discovery = md
		e.logf("paper engine: provider supports market discovery (legacy)")
	}

	// If the strategy implements SymbolResolver, let it discover/choose symbols.
	// This runs before recovery and OnInit — it determines what we trade.
	if resolver, ok := e.strategy.(strategy.SymbolResolver); ok {
		e.logf("paper engine: calling ResolveSymbols...")
		resolved, err := resolver.ResolveSymbols(strategy.InitContext{
			Symbols:   symbols,
			Timeframe: e.baseTimeframe,
			Config:    e.config.ConfigJSON,
			Search:    search,
			Discovery: discovery,
		})
		if err != nil {
			return fmt.Errorf("strategy ResolveSymbols: %w", err)
		}
		symbols = mergeUnique(symbols, resolved)
		e.logf("paper engine: resolved symbols: %v", symbols)
	}

	if len(symbols) == 0 {
		return fmt.Errorf("no symbols to trade — pass --symbols or implement SymbolResolver")
	}

	// If the provider supports contract specs (futures), query multipliers
	// for all symbols and configure the portfolio accordingly.
	e.contractSpecs = make(map[string]provider.ContractSpec)
	if csp, ok := e.md.(provider.ContractSpecProvider); ok {
		multipliers := make(map[string]float64)
		for _, sym := range symbols {
			spec, err := csp.GetContractSpec(ctx, sym)
			if err != nil {
				e.logf("paper engine: contract spec for %s: %v (defaulting to equity)", sym, err)
				continue
			}
			e.contractSpecs[sym] = spec
			if spec.PointValue > 1.0 {
				multipliers[sym] = spec.PointValue
				e.logf("paper engine: %s point_value=%.2f (tick_size=%.4f tick_value=%.4f)",
					sym, spec.PointValue, spec.TickSize, spec.TickValue)
			}
		}
		if len(multipliers) > 0 {
			e.portfolio.SetMultipliers(multipliers)
		}
	}

	// Pass contract specs to strategy if it supports it.
	if csc, ok := e.strategy.(strategy.ContractSpecConsumer); ok {
		stratSpecs := make(map[string]strategy.ContractSpec)
		for sym, spec := range e.contractSpecs {
			stratSpecs[sym] = strategy.ContractSpec{
				Symbol:     spec.Symbol,
				TickSize:   spec.TickSize,
				TickValue:  spec.TickValue,
				PointValue: spec.PointValue,
			}
		}
		csc.SetContractSpecs(stratSpecs)
	}

	// Pass runtime helpers (bar buffer, daily levels) to strategy.
	if rhc, ok := e.strategy.(strategy.RuntimeHelpersConsumer); ok {
		rhc.SetRuntimeHelpers(e.barBuffer, e.dailyTracker)
	}

	// Read timeframes from the strategy (single source of truth).
	timeframes := e.strategy.Timeframes()
	if len(timeframes) == 0 {
		return fmt.Errorf("strategy Timeframes() must return at least one timeframe")
	}
	sorted, err := SortTimeframes(timeframes)
	if err != nil {
		return fmt.Errorf("invalid timeframes from strategy: %w", err)
	}
	e.baseTimeframe = sorted[0]

	// Set up aggregators for higher timeframes (if any).
	if len(sorted) > 1 {
		for _, tf := range sorted[1:] {
			dur, _ := ParseTimeframe(tf) // already validated by SortTimeframes
			agg := NewBarAggregator(tf, dur, func(timeframe string, tick strategy.Tick) {
				e.handleBar(timeframe, tick)
			})
			e.aggregators = append(e.aggregators, agg)
		}
		e.logf("paper engine: multi-timeframe active | base=%s higher=%v", sorted[0], sorted[1:])
	}

	// Seed portfolio and warm up strategy indicators from account state + recent history.
	if err := e.recover(ctx, symbols); err != nil {
		return fmt.Errorf("startup recovery: %w", err)
	}

	// Recovery replaces the portfolio — re-apply futures multipliers so live
	// P&L is computed with the correct point values.
	if len(e.contractSpecs) > 0 {
		multipliers := make(map[string]float64)
		for sym, spec := range e.contractSpecs {
			if spec.PointValue > 1.0 {
				multipliers[sym] = spec.PointValue
			}
		}
		if len(multipliers) > 0 {
			e.portfolio.SetMultipliers(multipliers)
		}
	}

	// If the strategy implements Initializer, call OnInit before any market data.
	if init, ok := e.strategy.(strategy.Initializer); ok {
		e.logf("paper engine: calling OnInit...")
		if err := init.OnInit(strategy.InitContext{
			Symbols:    symbols,
			Timeframe:  e.baseTimeframe,
			Config:     e.config.ConfigJSON,
			Search:     search,
			Discovery:  discovery,
			AddSymbols: e.addSymbols,
		}); err != nil {
			return fmt.Errorf("strategy OnInit: %w", err)
		}
		e.logf("paper engine: OnInit complete")
	}

	// If the strategy implements Shutdowner, call OnExit when the engine stops.
	if sd, ok := e.strategy.(strategy.Shutdowner); ok {
		defer func() {
			e.logf("paper engine: calling OnExit...")
			sd.OnExit()
			e.logf("paper engine: OnExit complete")
		}()
	}

	// Fill events use a dedicated channel that blocks instead of dropping,
	// ensuring fills are never lost (portfolio state must stay in sync).
	go func() {
		if err := e.exec.SubscribeFills(ctx, func(f provider.Fill) {
			ev := fillEvent{fill: strategy.Fill{
				Symbol:    f.Symbol,
				Side:      f.Side,
				Qty:       f.Qty,
				Price:     f.Price,
				Timestamp: f.Timestamp,
			}}
			select {
			case e.fillCh <- ev:
			case <-ctx.Done():
			}
		}); err != nil && ctx.Err() == nil {
			e.logf("paper engine: fill subscription error: %v", err)
		}
	}()

	// If the strategy implements TradeSubscriber, subscribe to individual trades.
	if _, ok := e.strategy.(strategy.TradeSubscriber); ok {
		go func() {
			if err := e.md.SubscribeTrades(ctx, symbols, func(t provider.Trade) {
				e.send(ctx, tradeEvent{trade: strategy.Trade{
					Symbol:    t.Symbol,
					Timestamp: t.Timestamp,
					Price:     t.Price,
					Size:      uint32(t.Size),
				}})
			}); err != nil && ctx.Err() == nil {
				e.logf("paper engine: trade subscription error: %v", err)
			}
		}()
		e.logf("paper engine: trade-level subscription active for %v", symbols)
	}

	// If the strategy implements QuoteSubscriber, subscribe to bid/ask quotes.
	if _, ok := e.strategy.(strategy.QuoteSubscriber); ok {
		go func() {
			if err := e.md.SubscribeQuotes(ctx, symbols, func(q provider.Quote) {
				e.send(ctx, quoteEvent{quote: strategy.Quote{
					Symbol:    q.Symbol,
					Timestamp: q.Timestamp,
					BidPrice:  q.BidPrice,
					BidSize:   q.BidSize,
					AskPrice:  q.AskPrice,
					AskSize:   q.AskSize,
				}})
			}); err != nil && ctx.Err() == nil {
				e.logf("paper engine: quote subscription error: %v", err)
			}
		}()
		e.logf("paper engine: quote-level subscription active for %v", symbols)
	}

	// If the strategy implements DailySessionHandler, subscribe to market
	// open/close events. Skip entirely if the provider declares continuous
	// trading (e.g. prediction markets, crypto). Otherwise prefer provider-driven
	// events if available, falling back to clock-based scheduling.
	if _, ok := e.strategy.(strategy.DailySessionHandler); ok {
		if _, continuous := e.md.(provider.ContinuousMarket); continuous {
			e.logf("paper engine: provider is a continuous market — session hooks disabled")
		} else if sn, ok := e.md.(provider.SessionNotifier); ok {
			go func() {
				if err := sn.SubscribeSession(ctx, func(ev provider.SessionEvent) {
					switch ev.Type {
					case "market_open":
						e.send(ctx, marketOpenEvent{})
					case "market_close":
						e.send(ctx, marketCloseEvent{})
					}
				}); err != nil && ctx.Err() == nil {
					e.logf("paper engine: session subscription error: %v", err)
				}
			}()
			e.logf("paper engine: market session subscription active (provider-driven)")
		} else {
			sched := e.config.MarketSchedule
			if sched == nil {
				sched = NYSESchedule()
			}
			go e.runSessionClock(ctx, sched)
			e.logf("paper engine: market session subscription active (clock-based: open=%s close=%s tz=%s)",
				sched.Open, sched.Close, sched.Timezone)
		}
	}

	// Initialize stats and start periodic status logging.
	e.stats = engineStats{
		lastBarTime: make(map[string]time.Time),
		startTime:   time.Now(),
	}
	if e.config.StatusInterval >= 0 {
		go e.statusLogger(ctx)
	}

	go e.processLoop(ctx)

	e.logf("paper engine: connecting to bar stream | symbols=%v timeframe=%s", symbols, e.baseTimeframe)

	// SubscribeBars blocks until ctx is cancelled — this is the main run loop.
	return e.md.SubscribeBars(ctx, symbols, e.baseTimeframe, func(b provider.Bar) {
		e.send(ctx, tickEvent{tick: provider.BarToTick(b)})
	})
}

// send routes an event to the processing loop, dropping it with a warning if
// the channel is full (avoids blocking the WebSocket callback goroutine).
func (e *Engine) send(ctx context.Context, ev engineEvent) {
	select {
	case e.eventCh <- ev:
	case <-ctx.Done():
	default:
		e.logf("warning: event channel full, dropping %T event", ev)
	}
}

func (e *Engine) processLoop(ctx context.Context) {
	for {
		// Prioritize fills over other events to keep portfolio in sync.
		select {
		case <-ctx.Done():
			return
		case f := <-e.fillCh:
			e.onFill(f.fill)
			continue
		default:
		}

		select {
		case <-ctx.Done():
			return
		case f := <-e.fillCh:
			e.onFill(f.fill)
		case ev := <-e.eventCh:
			switch v := ev.(type) {
			case tickEvent:
				e.handleBar(e.baseTimeframe, v.tick)
			case tradeEvent:
				e.onTrade(v.trade)
			case quoteEvent:
				e.onQuote(v.quote)
			case marketOpenEvent:
				e.onMarketOpen()
			case marketCloseEvent:
				e.onMarketClose()
			}
		}
	}
}

func (e *Engine) handleBar(timeframe string, tick strategy.Tick) {
	if !e.warmingUp && timeframe == e.baseTimeframe {
		e.stats.barsReceived++
		e.stats.lastBarTime[tick.Symbol] = tick.Timestamp
		e.stats.lastBarAt = time.Now()
	}

	e.portfolio.UpdateMarketPrice(tick.Symbol, tick.Close)
	if timeframe == e.baseTimeframe {
		e.barBuffer.Push(tick)
		date := tick.Timestamp.UTC().Format("2006-01-02")
		e.dailyTracker.Update(tick.Symbol, date, tick.Open, tick.High, tick.Low, tick.Close)
		if !e.warmingUp {
			e.portfolio.IncrementHoldingBars(tick.Symbol)
		}
	}
	e.strategy.SetPortfolio(e.portfolio)
	orders := e.strategy.OnBar(timeframe, tick)
	if e.warmingUp {
		e.checkWarmupBrackets(tick)
		simulateFills(e.strategy, e.portfolio, orders, tick, e)
	} else {
		e.checkStops(tick.Symbol, tick.Close)
		e.submitOrders(orders)
	}

	// Only feed base timeframe bars through aggregators.
	if timeframe == e.baseTimeframe {
		for _, agg := range e.aggregators {
			agg.Update(tick)
		}
	}
}

func (e *Engine) onTrade(trade strategy.Trade) {
	e.stats.tradesReceived++
	e.portfolio.UpdateMarketPrice(trade.Symbol, trade.Price)
	e.checkStops(trade.Symbol, trade.Price)
	ts := e.strategy.(strategy.TradeSubscriber) // safe: only called when strategy implements it
	e.strategy.SetPortfolio(e.portfolio)
	e.submitOrders(ts.OnTrade(trade))
}

func (e *Engine) onQuote(quote strategy.Quote) {
	e.stats.quotesReceived++
	qs := e.strategy.(strategy.QuoteSubscriber) // safe: only called when strategy implements it
	e.strategy.SetPortfolio(e.portfolio)
	e.submitOrders(qs.OnQuote(quote))
}

func (e *Engine) onMarketOpen() {
	dsh := e.strategy.(strategy.DailySessionHandler) // safe: only called when strategy implements it
	e.logf("paper engine: market open — calling OnMarketOpen")
	e.portfolio.ResetDaily()
	e.strategy.SetPortfolio(e.portfolio)
	e.submitOrders(dsh.OnMarketOpen())
}

func (e *Engine) onMarketClose() {
	dsh := e.strategy.(strategy.DailySessionHandler) // safe: only called when strategy implements it
	e.logf("paper engine: market close — calling OnMarketClose")
	e.strategy.SetPortfolio(e.portfolio)
	e.submitOrders(dsh.OnMarketClose())

	// Auto-flatten any remaining positions if configured.
	if e.config.FlattenAtClose {
		e.flattenAll("market close auto-flatten")
	}
}

// flattenAll closes all open positions with market orders.
func (e *Engine) flattenAll(reason string) {
	for _, o := range orderutil.BuildFlattenOrders(e.portfolio.Positions(), reason) {
		e.logf("auto-flatten: %s %s qty=%.2f reason=%q", o.Side, o.Symbol, o.Qty, reason)
		e.placeOrder(o)
	}
}

func (e *Engine) onFill(fill strategy.Fill) {
	e.stats.fillsReceived++

	// Compute per-fill realized P&L and classify side BEFORE applying.
	fill.RealizedPL = e.portfolio.ComputeFillPL(fill)
	fill.Side = e.portfolio.ClassifyFillSide(fill)

	e.portfolio.ApplyFill(fill)
	e.portfolio.UpdateMarketPrice(fill.Symbol, fill.Price)
	e.strategy.SetPortfolio(e.portfolio)
	e.strategy.OnFill(fill)

	e.logf("fill: %s %s qty=%.2f @ $%.2f pl=%.4f", fill.Side, fill.Symbol, fill.Qty, fill.Price, fill.RealizedPL)

	if err := e.store.LogFill(fill); err != nil {
		e.logf("db: could not log fill: %v", err)
	}
	if err := e.store.LogSnapshot(e.portfolio.Cash(), e.portfolio.Equity(), e.portfolio.TotalPL()); err != nil {
		e.logf("db: could not log snapshot: %v", err)
	}
}

// runSessionClock fires marketOpenEvent and marketCloseEvent based on wall-clock
// time. It skips weekends (Saturday/Sunday) and sleeps until the next event.
func (e *Engine) runSessionClock(ctx context.Context, sched *MarketSchedule) {
	loc, err := time.LoadLocation(sched.Timezone)
	if err != nil {
		e.logf("paper engine: invalid market timezone %q: %v — session clock disabled", sched.Timezone, err)
		return
	}

	openH, openM := parseHHMM(sched.Open)
	closeH, closeM := parseHHMM(sched.Close)

	for {
		now := time.Now().In(loc)
		openTime, closeTime := nextSessionTimes(now, openH, openM, closeH, closeM, loc)

		// Decide which event comes next.
		var nextTime time.Time
		var ev engineEvent
		if now.Before(openTime) {
			nextTime = openTime
			ev = marketOpenEvent{}
		} else if now.Before(closeTime) {
			nextTime = closeTime
			ev = marketCloseEvent{}
		} else {
			// Past today's close — advance to next weekday's open.
			tomorrow := now.AddDate(0, 0, 1)
			openTime, _ = nextSessionTimes(tomorrow, openH, openM, closeH, closeM, loc)
			nextTime = openTime
			ev = marketOpenEvent{}
		}

		delay := time.Until(nextTime)
		e.logf("paper engine: next session event (%T) in %s at %s",
			ev, delay.Round(time.Second), nextTime.Format("2006-01-02 15:04:05 MST"))

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
			e.send(ctx, ev)
		}
	}
}

// nextSessionTimes returns today's (or the next weekday's) open and close
// times, skipping Saturday and Sunday.
func nextSessionTimes(now time.Time, openH, openM, closeH, closeM int, loc *time.Location) (open, close time.Time) {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	// Skip to Monday if on a weekend.
	for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
		day = day.AddDate(0, 0, 1)
	}

	open = time.Date(day.Year(), day.Month(), day.Day(), openH, openM, 0, 0, loc)
	close = time.Date(day.Year(), day.Month(), day.Day(), closeH, closeM, 0, 0, loc)
	return
}

// parseHHMM parses a "HH:MM" string into hour and minute ints.
func parseHHMM(s string) (int, int) {
	var h, m int
	fmt.Sscanf(s, "%d:%d", &h, &m)
	return h, m
}

func (e *Engine) submitOrders(orders []strategy.Order) {
	orders = e.validateOrders(orders)
	for _, order := range orders {

		// Intercept stop/stop_limit orders when provider needs client-side management.
		if e.clientSideStops && (order.OrderType == "stop" || order.OrderType == "stop_limit") {
			e.pendingStops = append(e.pendingStops, pendingStop{order: order})
			e.logf("stop order queued (client-side): %s %s qty=%.2f stop=$%.4f reason=%q",
				order.Side, order.Symbol, order.Qty, order.StopPrice, order.Reason)
			if err := e.store.LogOrder(order, "client-side-pending"); err != nil {
				e.logf("db: could not log order: %v", err)
			}
			continue
		}
		e.placeOrder(order)
	}
}

// validateOrders applies engine-level guards, dropping invalid or duplicate orders.
func (e *Engine) validateOrders(orders []strategy.Order) []strategy.Order {
	return orderutil.ValidateOrders(orders, e.config.MaxContracts, func(reason string) {
		e.logf("guard: %s", reason)
	})
}

// checkWarmupBrackets simulates TP/SL bracket fills during warmup.
func (e *Engine) checkWarmupBrackets(tick strategy.Tick) {
	remaining, fills := bracket.CheckAll(e.warmupBrackets, tick.Symbol, tick.High, tick.Low)
	e.warmupBrackets = remaining
	for _, fill := range fills {
		fill.Timestamp = tick.Timestamp
		e.portfolio.ApplyFill(fill)
		e.strategy.SetPortfolio(e.portfolio)
		e.strategy.OnFill(fill)
	}
}

// multiplier returns the point value for a symbol (1.0 for equities).
func (e *Engine) multiplier(symbol string) float64 {
	if spec, ok := e.contractSpecs[symbol]; ok && spec.PointValue > 0 {
		return spec.PointValue
	}
	return 1.0
}

func (e *Engine) placeOrder(order strategy.Order) {
	result, err := e.exec.PlaceOrder(e.ctx, order)
	if err != nil {
		e.stats.ordersErrored++
		e.logf("order error: %v", err)
		return
	}
	e.stats.ordersPlaced++
	e.logf("order placed: %s %s qty=%.2f reason=%q id=%s",
		order.Side, order.Symbol, order.Qty, order.Reason, result.ID)

	if err := e.store.LogOrder(order, result.ID); err != nil {
		e.logf("db: could not log order: %v", err)
	}
}

// checkStops evaluates pending client-side stop orders against the current price.
// Sell stops trigger when price drops to or below the stop price.
// Buy stops trigger when price rises to or above the stop price.
func (e *Engine) checkStops(symbol string, price float64) {
	if len(e.pendingStops) == 0 {
		return
	}

	remaining := e.pendingStops[:0] // reuse backing array
	for _, ps := range e.pendingStops {
		if ps.order.Symbol != symbol {
			remaining = append(remaining, ps)
			continue
		}

		triggered := false
		if ps.order.Side == "sell" && price <= ps.order.StopPrice {
			triggered = true
		} else if ps.order.Side == "buy" && price >= ps.order.StopPrice {
			triggered = true
		}

		if !triggered {
			remaining = append(remaining, ps)
			continue
		}

		// Convert to market or limit order and submit.
		submit := ps.order
		if submit.OrderType == "stop" {
			submit.OrderType = "market"
		} else { // stop_limit
			submit.OrderType = "limit"
		}
		submit.StopPrice = 0
		submit.Reason = fmt.Sprintf("stop triggered @ $%.4f: %s", price, ps.order.Reason)

		e.logf("stop triggered: %s %s stop=$%.4f price=$%.4f",
			ps.order.Side, ps.order.Symbol, ps.order.StopPrice, price)
		e.placeOrder(submit)
	}
	e.pendingStops = remaining
}

// statusLogger prints periodic status updates so operators can confirm the
// engine is alive, receiving data, and the strategy is healthy.
func (e *Engine) statusLogger(ctx context.Context) {
	interval := e.config.StatusInterval
	if interval == 0 {
		interval = 60 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastBars, lastTrades, lastQuotes, lastFills, lastOrders int64

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			snap := e.Snapshot()

			// Deliver structured snapshot to callback if configured.
			if e.config.OnStatus != nil {
				e.config.OnStatus(snap)
			}

			s := &e.stats

			// Compute rates since last log.
			newBars := snap.BarsReceived - lastBars
			newTrades := snap.TradesReceived - lastTrades
			newQuotes := snap.QuotesReceived - lastQuotes
			newFills := snap.FillsReceived - lastFills
			newOrders := snap.OrdersPlaced - lastOrders
			lastBars = snap.BarsReceived
			lastTrades = snap.TradesReceived
			lastQuotes = snap.QuotesReceived
			lastFills = snap.FillsReceived
			lastOrders = snap.OrdersPlaced

			// Per-symbol last bar age.
			var symbolStatus []string
			for sym, ss := range snap.Symbols {
				age := time.Since(ss.LastBarTime).Round(time.Second)
				label := fmt.Sprintf("%s=%s ago", sym, age)
				symbolStatus = append(symbolStatus, label)
			}

			// Data gap warning.
			sinceLastBar := ""
			if !s.lastBarAt.IsZero() {
				sinceLastBar = fmt.Sprintf(" | last_bar_wall=%s ago", time.Since(s.lastBarAt).Round(time.Second))
			}

			uptime := snap.Uptime.Round(time.Second)

			e.logf("status: uptime=%s bars=%d(+%d) trades=%d(+%d) quotes=%d(+%d) fills=%d(+%d) orders=%d(+%d) errors=%d%s",
				uptime, snap.BarsReceived, newBars, snap.TradesReceived, newTrades,
				snap.QuotesReceived, newQuotes, snap.FillsReceived, newFills,
				snap.OrdersPlaced, newOrders, snap.OrdersErrored, sinceLastBar)

			if len(symbolStatus) > 0 {
				e.logf("status: last_bar_data: %s", joinStrings(symbolStatus, ", "))
			}

			p := snap.Portfolio
			e.logf("status: portfolio cash=$%.2f equity=$%.2f pl=$%.2f open_positions=%d",
				p.Cash, p.Equity, p.TotalPL, len(p.Positions))

			for _, pos := range p.Positions {
				e.logf("status:   %s qty=%.2f avg=$%.2f mkt=$%.2f upl=$%.2f",
					pos.Symbol, pos.Qty, pos.AvgCost, pos.MarketValue, pos.UnrealizedPL)
			}

			if len(snap.RuntimeErrors) > 0 {
				e.logf("status: strategy has %d runtime error(s):", len(snap.RuntimeErrors))
				for _, err := range snap.RuntimeErrors {
					e.logf("status:   %s", err)
				}
			}

			// Warn if no bars received recently.
			if snap.BarsReceived > 0 && newBars == 0 {
				e.logf("status: WARNING — no new bars in the last 60s")
			}
			if snap.BarsReceived == 0 && uptime > 2*time.Minute {
				e.logf("status: WARNING — no bars received since startup")
			}
		}
	}
}

func joinStrings(ss []string, sep string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += sep
		}
		result += s
	}
	return result
}

// addSymbols dynamically subscribes to new symbols mid-run. Each call
// launches independent subscription goroutines that feed events into the
// existing event channel — no provider interface changes needed.
func (e *Engine) addSymbols(symbols ...string) {
	e.logf("paper engine: dynamically adding symbols %v", symbols)

	// Subscribe to bars for new symbols.
	go func() {
		if err := e.md.SubscribeBars(e.ctx, symbols, e.baseTimeframe, func(b provider.Bar) {
			e.send(e.ctx, tickEvent{tick: provider.BarToTick(b)})
		}); err != nil && e.ctx.Err() == nil {
			e.logf("paper engine: dynamic bar subscription error for %v: %v", symbols, err)
		}
	}()

	// Mirror trade subscriptions if strategy uses them.
	if _, ok := e.strategy.(strategy.TradeSubscriber); ok {
		go func() {
			if err := e.md.SubscribeTrades(e.ctx, symbols, func(t provider.Trade) {
				e.send(e.ctx, tradeEvent{trade: strategy.Trade{
					Symbol:    t.Symbol,
					Timestamp: t.Timestamp,
					Price:     t.Price,
					Size:      uint32(t.Size),
				}})
			}); err != nil && e.ctx.Err() == nil {
				e.logf("paper engine: dynamic trade subscription error for %v: %v", symbols, err)
			}
		}()
	}

	// Mirror quote subscriptions if strategy uses them.
	if _, ok := e.strategy.(strategy.QuoteSubscriber); ok {
		go func() {
			if err := e.md.SubscribeQuotes(e.ctx, symbols, func(q provider.Quote) {
				e.send(e.ctx, quoteEvent{quote: strategy.Quote{
					Symbol:    q.Symbol,
					Timestamp: q.Timestamp,
					BidPrice:  q.BidPrice,
					BidSize:   q.BidSize,
					AskPrice:  q.AskPrice,
					AskSize:   q.AskSize,
				}})
			}); err != nil && e.ctx.Err() == nil {
				e.logf("paper engine: dynamic quote subscription error for %v: %v", symbols, err)
			}
		}()
	}
}

// mergeUnique appends b items to a, skipping duplicates already in a.
func mergeUnique(a, b []string) []string {
	seen := make(map[string]bool, len(a))
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			a = append(a, s)
			seen[s] = true
		}
	}
	return a
}
