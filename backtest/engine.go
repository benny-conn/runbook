package backtest

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/benny-conn/brandon-bot/engine"
	"github.com/benny-conn/brandon-bot/internal/barbuf"
	"github.com/benny-conn/brandon-bot/internal/bracket"
	"github.com/benny-conn/brandon-bot/internal/portfolio"
	"github.com/benny-conn/brandon-bot/strategy"
)

// Trade records a completed fill along with realized P&L (for sells).
type Trade struct {
	Fill       strategy.Fill
	RealizedPL float64 // non-zero only for sell fills
}

// Diagnostics tracks internal engine metrics for debugging zero-trade backtests.
type Diagnostics struct {
	BarsProcessed    int      `json:"barsProcessed"`
	OrdersRejected   int      `json:"ordersRejected"`
	RejectionReasons []string `json:"rejectionReasons,omitempty"`
	reasonSeen       map[string]bool
}

const maxRejectionReasons = 10

func (d *Diagnostics) trackRejection(reason string) {
	d.OrdersRejected++
	if d.reasonSeen == nil {
		d.reasonSeen = make(map[string]bool)
	}
	if d.reasonSeen[reason] || len(d.RejectionReasons) >= maxRejectionReasons {
		return
	}
	d.reasonSeen[reason] = true
	d.RejectionReasons = append(d.RejectionReasons, reason)
}

// Results holds the output of a completed backtest run.
type Results struct {
	InitialCapital float64
	FinalEquity    float64
	TotalReturnPct float64
	MaxDrawdownPct float64
	SharpeRatio    float64 // per-bar, not annualized
	TotalTrades    int
	WinningTrades  int
	LosingTrades   int
	Trades         []Trade
	EquityCurve    []float64 // per-tick equity values
	Diagnostics    Diagnostics
}

func (r *Results) Print() {
	winRate := 0.0
	if r.TotalTrades > 0 {
		winRate = float64(r.WinningTrades) / float64(r.TotalTrades) * 100
	}
	fmt.Printf("\n=== Backtest Results ===\n")
	fmt.Printf("Initial capital:  $%.2f\n", r.InitialCapital)
	fmt.Printf("Final equity:     $%.2f\n", r.FinalEquity)
	fmt.Printf("Total return:     %.2f%%\n", r.TotalReturnPct)
	fmt.Printf("Max drawdown:     %.2f%%\n", r.MaxDrawdownPct)
	fmt.Printf("Sharpe ratio:     %.4f (per-bar, not annualized)\n", r.SharpeRatio)
	fmt.Printf("Total trades:     %d\n", r.TotalTrades)
	fmt.Printf("Win rate:         %.1f%% (%d W / %d L)\n", winRate, r.WinningTrades, r.LosingTrades)
	fmt.Printf("\n--- Trade Log ---\n")
	for _, t := range r.Trades {
		f := t.Fill
		if f.Side == "sell" {
			fmt.Printf("[%s] SELL %s  qty=%.2f  price=$%.2f  realizedPL=$%.2f\n",
				f.Timestamp.Format("2006-01-02 15:04"), f.Symbol, f.Qty, f.Price, t.RealizedPL)
		} else {
			fmt.Printf("[%s] BUY  %s  qty=%.2f  price=$%.2f\n",
				f.Timestamp.Format("2006-01-02 15:04"), f.Symbol, f.Qty, f.Price)
		}
	}
}

// Progress reports backtest run state, delivered via the OnProgress callback.
type Progress struct {
	BarsProcessed int
	TotalBars     int
	Elapsed       time.Duration
	Cash          float64
	Equity        float64
	TotalPL       float64
	OpenPositions int
	TradeCount    int
}

// EngineOption configures optional backtest engine behavior.
type EngineOption func(*Engine)

// WithMultipliers sets per-symbol contract multipliers for futures P&L.
// For equities, multiplier is 1.0 (default). For MNQ it's 2.0, ES is 50.0, etc.
// When a symbol has multiplier > 1, the cash check uses futures semantics
// (no notional cost) and P&L = price_diff × qty × multiplier.
func WithMultipliers(m map[string]float64) EngineOption {
	return func(e *Engine) {
		e.multipliers = m
		e.portfolio.SetMultipliers(m)
	}
}

// WithFlattenAtClose enables auto-flattening all positions at each day boundary.
func WithFlattenAtClose() EngineOption {
	return func(e *Engine) {
		e.flattenAtClose = true
	}
}

// WithLogger sets a custom logger for backtest engine output.
// If not set, the standard library log package is used.
func WithLogger(l engine.Logger) EngineOption {
	return func(e *Engine) {
		e.logger = l
	}
}

// WithOnProgress sets a callback invoked every n bars during the backtest.
// Provides consumers with visibility into long-running backtests.
// Set n to 0 to fire on every bar (not recommended for large datasets).
func WithOnProgress(n int, fn func(Progress)) EngineOption {
	return func(e *Engine) {
		e.progressInterval = n
		e.onProgress = fn
	}
}

// Engine runs a strategy against a sorted slice of historical ticks.
type Engine struct {
	strategy         strategy.Strategy
	portfolio        *portfolio.SimulatedPortfolio
	diagnostics      Diagnostics
	multipliers      map[string]float64 // per-symbol point value (nil = all equities)
	brackets         []bracket.Pending  // active TP/SL brackets
	flattenAtClose   bool
	barBuffer        *barbuf.Buffer
	dailyTracker     *barbuf.DailyTracker
	logger           engine.Logger
	onProgress       func(Progress)
	progressInterval int // fire onProgress every N bars (0 = every bar)
}

func NewEngine(strat strategy.Strategy, initialCapital float64, opts ...EngineOption) *Engine {
	e := &Engine{
		strategy:     strat,
		portfolio:    portfolio.NewSimulatedPortfolio(initialCapital),
		barBuffer:    barbuf.New(),
		dailyTracker: barbuf.NewDailyTracker(),
	}
	for _, opt := range opts {
		opt(e)
	}
	if e.logger == nil {
		e.logger = engine.DefaultLogger()
	}
	return e
}

// logf logs a formatted message through the configured logger.
func (e *Engine) logf(format string, v ...any) {
	e.logger.Printf(format, v...)
}

// tickSizeForSymbol returns the minimum tick size for common futures symbols.
// Falls back to 0.25 for unknown futures.
func tickSizeForSymbol(sym string) float64 {
	switch sym {
	case "MNQ", "NQ":
		return 0.25
	case "MES", "ES":
		return 0.25
	case "MYM", "YM":
		return 1.0
	case "M2K", "RTY":
		return 0.10
	case "MCL", "CL":
		return 0.01
	case "MGC", "GC":
		return 0.10
	default:
		return 0.25
	}
}

// multiplier returns the point value for a symbol (1.0 for equities).
func (e *Engine) multiplier(symbol string) float64 {
	if e.multipliers != nil {
		if m, ok := e.multipliers[symbol]; ok && m > 0 {
			return m
		}
	}
	return 1.0
}

// checkBrackets evaluates pending TP/SL brackets against the current tick.
func (e *Engine) checkBrackets(tick strategy.Tick) []Trade {
	remaining, fills := bracket.CheckAll(e.brackets, tick.Symbol, tick.High, tick.Low)
	e.brackets = remaining

	var trades []Trade
	for _, fill := range fills {
		fill.Timestamp = tick.Timestamp

		mult := e.multiplier(fill.Symbol)
		var realizedPL float64
		pos := e.portfolio.Position(fill.Symbol)
		if pos != nil {
			if fill.Side == "sell" && pos.Qty > 0 {
				realizedPL = (fill.Price - pos.AvgCost) * fill.Qty * mult
			} else if fill.Side == "buy" && pos.Qty < 0 {
				realizedPL = (pos.AvgCost - fill.Price) * fill.Qty * mult
			}
		}

		e.portfolio.ApplyFill(fill)
		e.strategy.SetPortfolio(e.portfolio)
		e.strategy.OnFill(fill)
		trades = append(trades, Trade{Fill: fill, RealizedPL: realizedPL})
	}
	return trades
}

// flattenAll closes all open positions at the given prices.
func (e *Engine) flattenAll(prices map[string]float64, fillTime time.Time) []Trade {
	var orders []strategy.Order
	for _, pos := range e.portfolio.Positions() {
		if pos.Qty == 0 {
			continue
		}
		side := "sell"
		qty := pos.Qty
		if pos.Qty < 0 {
			side = "buy"
			qty = -pos.Qty
		}
		orders = append(orders, strategy.Order{
			Symbol:    pos.Symbol,
			Side:      side,
			Qty:       qty,
			OrderType: "market",
			Reason:    "flatten at close",
		})
	}
	if len(orders) == 0 {
		return nil
	}
	return e.fillOrders(orders, prices, fillTime)
}

// validateOrders filters out invalid orders, matching the live engine's guards.
func (e *Engine) validateOrders(orders []strategy.Order) []strategy.Order {
	valid := make([]strategy.Order, 0, len(orders))
	for _, o := range orders {
		if o.Symbol == "" || o.Qty <= 0 {
			e.diagnostics.trackRejection(fmt.Sprintf("invalid order: symbol=%q qty=%.2f", o.Symbol, o.Qty))
			continue
		}
		valid = append(valid, o)
	}
	return valid
}

// fillOrders simulates fills for a set of orders using per-symbol prices.
// Each order fills at the price from symbolPrices for its symbol; orders
// for symbols not in the map are skipped.
func (e *Engine) fillOrders(orders []strategy.Order, symbolPrices map[string]float64, fillTime time.Time) []Trade {
	orders = e.validateOrders(orders)
	var trades []Trade
	for _, order := range orders {
		fillPrice, ok := symbolPrices[order.Symbol]
		if !ok || fillPrice <= 0 {
			e.diagnostics.trackRejection(fmt.Sprintf("%s: no price data available", order.Symbol))
			continue
		}

		var realizedPL float64
		fillQty := order.Qty
		mult := e.multiplier(order.Symbol)

		if order.Side == "buy" {
			pos := e.portfolio.Position(order.Symbol)
			if pos != nil && pos.Qty < 0 {
				shortQty := -pos.Qty
				if fillQty > shortQty {
					fillQty = shortQty
				}
				realizedPL = (pos.AvgCost - fillPrice) * fillQty * mult
			} else if mult > 1.0 {
				// Futures — no notional cash check. Scripts enforce contract limits.
			} else {
				// Equities — check cash.
				cost := fillQty * fillPrice
				if cost > e.portfolio.Cash() {
					e.diagnostics.trackRejection(fmt.Sprintf("%s buy: cost $%.2f exceeds cash $%.2f", order.Symbol, cost, e.portfolio.Cash()))
					continue
				}
			}
		}

		if order.Side == "sell" {
			pos := e.portfolio.Position(order.Symbol)
			if pos != nil && pos.Qty > 0 {
				if fillQty > pos.Qty {
					fillQty = pos.Qty
				}
				realizedPL = (fillPrice - pos.AvgCost) * fillQty * mult
			}
		}

		fill := strategy.Fill{
			Symbol:    order.Symbol,
			Side:      order.Side,
			Qty:       fillQty,
			Price:     fillPrice,
			Timestamp: fillTime,
		}

		e.portfolio.ApplyFill(fill)
		e.strategy.SetPortfolio(e.portfolio)
		e.strategy.OnFill(fill)

		trades = append(trades, Trade{Fill: fill, RealizedPL: realizedPL})

		// Register TP/SL bracket for simulation on subsequent ticks.
		if b := bracket.NewFromOrder(order, fillPrice); b != nil {
			e.brackets = append(e.brackets, *b)
		}
	}
	return trades
}

// fillOrdersCross builds per-symbol fill prices from the next available tick for
// each order's symbol, supporting cross-symbol orders (e.g. an AAPL tick's
// strategy returning a MSFT order). Falls back to the current tick's next bar
// for same-symbol orders (fast path).
func (e *Engine) fillOrdersCross(orders []strategy.Order, tickIdx int, ticks []strategy.Tick, nextSameSymbol []int, symIdx symbolTickIndices) []Trade {
	tickPrices := make(map[string]float64)
	var fillTime time.Time

	// Determine the fill price and time for each unique order symbol.
	currentSym := ticks[tickIdx].Symbol
	for _, o := range orders {
		if _, ok := tickPrices[o.Symbol]; ok {
			continue // already resolved
		}
		var nextIdx int
		if o.Symbol == currentSym {
			// Fast path: use precomputed next-same-symbol index.
			nextIdx = nextSameSymbol[tickIdx]
		} else {
			// Cross-symbol: find next tick for this order's symbol after current tick.
			nextIdx = symIdx.nextTickAfter(o.Symbol, tickIdx)
		}
		if nextIdx != -1 {
			tickPrices[o.Symbol] = ticks[nextIdx].Open
			if fillTime.IsZero() || ticks[nextIdx].Timestamp.After(fillTime) {
				fillTime = ticks[nextIdx].Timestamp
			}
		}
	}

	if len(tickPrices) == 0 {
		return nil
	}
	// Use earliest fill time if none resolved (shouldn't happen).
	if fillTime.IsZero() {
		fillTime = ticks[tickIdx].Timestamp
	}

	return e.fillOrders(orders, tickPrices, fillTime)
}

// dayPrices holds precomputed open/close prices for all symbols on a given day.
type dayPrices struct {
	date      string
	opens     map[string]float64 // symbol → open price
	closes    map[string]float64 // symbol → close price
	firstTime time.Time          // timestamp of the first tick on this day
}

// precomputeDayPrices groups ticks by calendar date and extracts per-symbol
// open (first tick) and close (last tick) prices for each day. This allows
// lifecycle hooks to fill at correct per-symbol prices for the entire day,
// regardless of which symbol's tick triggers the day boundary.
func precomputeDayPrices(ticks []strategy.Tick) []dayPrices {
	var days []dayPrices
	var current *dayPrices

	for _, tick := range ticks {
		date := tick.Timestamp.UTC().Format("2006-01-02")
		if current == nil || current.date != date {
			if current != nil {
				days = append(days, *current)
			}
			current = &dayPrices{
				date:      date,
				opens:     make(map[string]float64),
				closes:    make(map[string]float64),
				firstTime: tick.Timestamp,
			}
		}
		// First tick for this symbol on this day → open price.
		if _, seen := current.opens[tick.Symbol]; !seen {
			current.opens[tick.Symbol] = tick.Open
		}
		// Always update close to the latest tick for this symbol on this day.
		current.closes[tick.Symbol] = tick.Close
	}
	if current != nil {
		days = append(days, *current)
	}
	return days
}

// recordEquity snapshots the current equity and updates drawdown tracking.
func recordEquity(equity float64, equityCurve *[]float64, peakEquity, maxDrawdown *float64) {
	*equityCurve = append(*equityCurve, equity)
	if equity > *peakEquity {
		*peakEquity = equity
	}
	if *peakEquity > 0 {
		dd := (*peakEquity - equity) / *peakEquity * 100
		if dd > *maxDrawdown {
			*maxDrawdown = dd
		}
	}
}

// Run replays ticks chronologically, simulates fills at the next bar's open,
// and returns full performance metrics. If the strategy implements
// DailySessionHandler, OnMarketOpen/OnMarketClose are called at day boundaries
// with correct per-symbol prices.
func (e *Engine) Run(ticks []strategy.Tick) *Results {
	if len(ticks) == 0 {
		return &Results{InitialCapital: e.portfolio.Cash()}
	}

	initialCapital := e.portfolio.Cash()

	// Precompute: for each tick index, the index of the next tick for the same symbol.
	// Used to simulate fills at next bar's open.
	nextSameSymbol := precomputeNextSameSymbol(ticks)

	// Per-symbol tick index for cross-symbol order fills.
	symIndices := buildSymbolTickIndices(ticks)

	// Pass runtime helpers to strategy.
	if rhc, ok := e.strategy.(strategy.RuntimeHelpersConsumer); ok {
		rhc.SetRuntimeHelpers(e.barBuffer, e.dailyTracker)
	}

	// Pass contract specs to strategy so getContract() is available in scripts.
	if csc, ok := e.strategy.(strategy.ContractSpecConsumer); ok && len(e.multipliers) > 0 {
		specs := make(map[string]strategy.ContractSpec)
		for sym, pv := range e.multipliers {
			specs[sym] = strategy.ContractSpec{
				Symbol:     sym,
				TickSize:   tickSizeForSymbol(sym),
				TickValue:  tickSizeForSymbol(sym) * pv,
				PointValue: pv,
			}
		}
		csc.SetContractSpecs(specs)
	}

	// Call OnInit if the strategy supports it.
	if init, ok := e.strategy.(strategy.Initializer); ok {
		if err := init.OnInit(strategy.InitContext{}); err != nil {
			// Log but don't fail — backtest should still run.
			e.logf("warning: strategy OnInit error: %v", err)
		}
	}

	// Progress tracking.
	startedAt := time.Now()
	totalBars := len(ticks)
	progressInterval := e.progressInterval
	if progressInterval <= 0 {
		progressInterval = 1
	}

	// Check if the strategy supports daily session hooks.
	dsh, hasDailyHooks := e.strategy.(strategy.DailySessionHandler)

	// Precompute per-day prices for lifecycle hook fills.
	var days []dayPrices
	var dayIndex int // current position in the days slice
	if hasDailyHooks {
		days = precomputeDayPrices(ticks)
	}

	var (
		trades      []Trade
		equityCurve []float64
		peakEquity  float64
		maxDrawdown float64
		currentDate string
	)

	// Multi-timeframe support: set up aggregators when strategy declares >1 timeframe.
	var hasMTF bool
	var baseTimeframe string
	var aggregators []*engine.BarAggregator
	type completedBar struct {
		timeframe string
		tick      strategy.Tick
	}
	var completedBars []completedBar

	{
		timeframes := e.strategy.Timeframes()
		sorted, _ := engine.SortTimeframes(timeframes)
		if len(sorted) > 0 {
			baseTimeframe = sorted[0]
		}
		if len(sorted) > 1 {
			hasMTF = true
			for _, tf := range sorted[1:] {
				dur, _ := engine.ParseTimeframe(tf)
				agg := engine.NewBarAggregator(tf, dur, func(timeframe string, tick strategy.Tick) {
					completedBars = append(completedBars, completedBar{timeframe, tick})
				})
				aggregators = append(aggregators, agg)
			}
		}
	}

	for i, tick := range ticks {
		// Update market prices so Equity() stays accurate.
		e.portfolio.UpdateMarketPrice(tick.Symbol, tick.Close)
		e.barBuffer.Push(tick)
		e.dailyTracker.Update(tick.Symbol, tick.Timestamp.UTC().Format("2006-01-02"),
			tick.Open, tick.High, tick.Low, tick.Close)

		// Check pending TP/SL brackets against this tick's price range.
		bracketTrades := e.checkBrackets(tick)
		trades = append(trades, bracketTrades...)

		date := tick.Timestamp.UTC().Format("2006-01-02")

		// --- Day boundary handling for strategies with lifecycle hooks ---
		if hasDailyHooks && date != currentDate {
			if currentDate != "" {
				// End of previous day: fire OnMarketClose, then snapshot equity
				// so the curve reflects the effect of EOD orders (e.g. position flattening).
				prevDay := days[dayIndex]
				e.strategy.SetPortfolio(e.portfolio)
				closeOrders := dsh.OnMarketClose()
				if len(closeOrders) > 0 {
					trades = append(trades, e.fillOrders(closeOrders, prevDay.closes, tick.Timestamp)...)
				}
				// Auto-flatten remaining positions if configured.
				if e.flattenAtClose {
					trades = append(trades, e.flattenAll(prevDay.closes, tick.Timestamp)...)
				}
				recordEquity(e.portfolio.Equity(), &equityCurve, &peakEquity, &maxDrawdown)
				dayIndex++
			}

			// Start of new day: update all symbols to today's open prices,
			// then fire OnMarketOpen.
			currentDate = date
			newDay := days[dayIndex]
			for sym, openPrice := range newDay.opens {
				e.portfolio.UpdateMarketPrice(sym, openPrice)
			}
			e.portfolio.ResetDaily()
			e.strategy.SetPortfolio(e.portfolio)
			openOrders := dsh.OnMarketOpen()
			if len(openOrders) > 0 {
				trades = append(trades, e.fillOrders(openOrders, newDay.opens, tick.Timestamp)...)
			}
		} else if !hasDailyHooks {
			// Reset daily P&L at day boundaries even without daily hooks.
			if date != currentDate {
				if currentDate != "" && e.flattenAtClose {
					// Flatten at end of previous day.
					closePrices := make(map[string]float64)
					for _, pos := range e.portfolio.Positions() {
						closePrices[pos.Symbol] = e.portfolio.LastPrice(pos.Symbol)
					}
					trades = append(trades, e.flattenAll(closePrices, tick.Timestamp)...)
				}
				currentDate = date
				e.portfolio.ResetDaily()
			}
			// For non-daily strategies, record equity on every tick.
			recordEquity(e.portfolio.Equity(), &equityCurve, &peakEquity, &maxDrawdown)
		}

		// --- Ask the strategy what to do ---
		e.portfolio.IncrementHoldingBars(tick.Symbol)
		e.strategy.SetPortfolio(e.portfolio)
		orders := e.strategy.OnBar(baseTimeframe, tick)
		if len(orders) > 0 {
			trades = append(trades, e.fillOrdersCross(orders, i, ticks, nextSameSymbol, symIndices)...)
		}

		// --- Feed through aggregators for higher-timeframe bars ---
		if hasMTF {
			for _, agg := range aggregators {
				agg.Update(tick)
			}
			// Sort completed bars by timeframe duration (shortest first) so strategies
			// always see 5m before 1h, matching the live engine's behavior.
			if len(completedBars) > 1 {
				sort.Slice(completedBars, func(a, b int) bool {
					da, _ := engine.ParseTimeframe(completedBars[a].timeframe)
					db, _ := engine.ParseTimeframe(completedBars[b].timeframe)
					return da < db
				})
			}
			for _, cb := range completedBars {
				e.strategy.SetPortfolio(e.portfolio)
				barOrders := e.strategy.OnBar(cb.timeframe, cb.tick)
				if len(barOrders) > 0 {
					trades = append(trades, e.fillOrdersCross(barOrders, i, ticks, nextSameSymbol, symIndices)...)
				}
			}
			completedBars = completedBars[:0]
		}

		// Fire progress callback at the configured interval.
		if e.onProgress != nil && (i+1)%progressInterval == 0 {
			e.onProgress(Progress{
				BarsProcessed: i + 1,
				TotalBars:     totalBars,
				Elapsed:       time.Since(startedAt),
				Cash:          e.portfolio.Cash(),
				Equity:        e.portfolio.Equity(),
				TotalPL:       e.portfolio.TotalPL(),
				OpenPositions: len(e.portfolio.Positions()),
				TradeCount:    len(trades),
			})
		}
	}

	// Fire final market close if the strategy uses daily hooks.
	if hasDailyHooks && currentDate != "" {
		lastDay := days[dayIndex]
		e.strategy.SetPortfolio(e.portfolio)
		closeOrders := dsh.OnMarketClose()
		if len(closeOrders) > 0 {
			lastTick := ticks[len(ticks)-1]
			trades = append(trades, e.fillOrders(closeOrders, lastDay.closes, lastTick.Timestamp)...)
		}
		// Snapshot equity after EOD orders so the curve reflects final state.
		recordEquity(e.portfolio.Equity(), &equityCurve, &peakEquity, &maxDrawdown)
	}

	// Final equity snapshot.
	finalEquity := e.portfolio.Equity()
	recordEquity(finalEquity, &equityCurve, &peakEquity, &maxDrawdown)

	wins, losses := 0, 0
	for _, t := range trades {
		if t.RealizedPL != 0 {
			if t.RealizedPL > 0 {
				wins++
			} else {
				losses++
			}
		}
	}

	e.diagnostics.BarsProcessed = len(ticks)

	return &Results{
		InitialCapital: initialCapital,
		FinalEquity:    finalEquity,
		TotalReturnPct: (finalEquity - initialCapital) / initialCapital * 100,
		MaxDrawdownPct: maxDrawdown,
		SharpeRatio:    sharpe(equityCurve),
		TotalTrades:    wins + losses,
		WinningTrades:  wins,
		LosingTrades:   losses,
		Trades:         trades,
		EquityCurve:    equityCurve,
		Diagnostics:    e.diagnostics,
	}
}

// precomputeNextSameSymbol returns a slice where index i holds the index of the
// next tick with the same symbol as ticks[i], or -1 if none exists.
func precomputeNextSameSymbol(ticks []strategy.Tick) []int {
	next := make([]int, len(ticks))
	for i := range next {
		next[i] = -1
	}
	lastSeen := make(map[string]int)
	for i := len(ticks) - 1; i >= 0; i-- {
		sym := ticks[i].Symbol
		if j, ok := lastSeen[sym]; ok {
			next[i] = j
		}
		lastSeen[sym] = i
	}
	return next
}

// symbolTickIndices maps each symbol to its sorted list of tick indices.
// Used to find the next tick for any symbol at any point during replay.
type symbolTickIndices map[string][]int

func buildSymbolTickIndices(ticks []strategy.Tick) symbolTickIndices {
	idx := make(symbolTickIndices)
	for i, t := range ticks {
		idx[t.Symbol] = append(idx[t.Symbol], i)
	}
	return idx
}

// nextTickAfter returns the index of the first tick for sym that comes after
// position i, or -1 if none exists.
func (s symbolTickIndices) nextTickAfter(sym string, i int) int {
	indices := s[sym]
	// Binary search for first index > i.
	lo, hi := 0, len(indices)
	for lo < hi {
		mid := (lo + hi) / 2
		if indices[mid] <= i {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(indices) {
		return indices[lo]
	}
	return -1
}

// sharpe computes the per-bar Sharpe ratio from an equity curve (not annualized).
func sharpe(equityCurve []float64) float64 {
	if len(equityCurve) < 2 {
		return 0
	}
	returns := make([]float64, len(equityCurve)-1)
	for i := 1; i < len(equityCurve); i++ {
		if equityCurve[i-1] != 0 {
			returns[i-1] = (equityCurve[i] - equityCurve[i-1]) / equityCurve[i-1]
		}
	}
	mean := 0.0
	for _, r := range returns {
		mean += r
	}
	mean /= float64(len(returns))

	variance := 0.0
	for _, r := range returns {
		d := r - mean
		variance += d * d
	}
	variance /= float64(len(returns))
	std := math.Sqrt(variance)

	if std == 0 {
		return 0
	}
	return mean / std
}
