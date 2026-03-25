package backtest

import (
	"fmt"
	"math"
	"time"

	"github.com/benny-conn/brandon-bot/engine"
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

// Engine runs a strategy against a sorted slice of historical ticks.
type Engine struct {
	strategy    strategy.Strategy
	portfolio   *portfolio.SimulatedPortfolio
	diagnostics Diagnostics
	multipliers map[string]float64 // per-symbol point value (nil = all equities)
}

func NewEngine(strat strategy.Strategy, initialCapital float64, opts ...EngineOption) *Engine {
	e := &Engine{
		strategy:  strat,
		portfolio: portfolio.NewSimulatedPortfolio(initialCapital),
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
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

// fillOrders simulates fills for a set of orders using per-symbol prices.
// Each order fills at the price from symbolPrices for its symbol; orders
// for symbols not in the map are skipped.
func (e *Engine) fillOrders(orders []strategy.Order, symbolPrices map[string]float64, fillTime time.Time) []Trade {
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
		e.strategy.OnFill(fill)

		trades = append(trades, Trade{Fill: fill, RealizedPL: realizedPL})
	}
	return trades
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
		date := tick.Timestamp.Format("2006-01-02")
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

		date := tick.Timestamp.Format("2006-01-02")

		// --- Day boundary handling for strategies with lifecycle hooks ---
		if hasDailyHooks && date != currentDate {
			if currentDate != "" {
				// End of previous day: snapshot equity, then fire OnMarketClose.
				recordEquity(e.portfolio.Equity(), &equityCurve, &peakEquity, &maxDrawdown)

				prevDay := days[dayIndex]
				closeOrders := dsh.OnMarketClose(e.portfolio)
				if len(closeOrders) > 0 {
					trades = append(trades, e.fillOrders(closeOrders, prevDay.closes, tick.Timestamp)...)
				}
				dayIndex++
			}

			// Start of new day: update all symbols to today's open prices,
			// then fire OnMarketOpen.
			currentDate = date
			newDay := days[dayIndex]
			for sym, openPrice := range newDay.opens {
				e.portfolio.UpdateMarketPrice(sym, openPrice)
			}
			openOrders := dsh.OnMarketOpen(e.portfolio)
			if len(openOrders) > 0 {
				trades = append(trades, e.fillOrders(openOrders, newDay.opens, tick.Timestamp)...)
			}
		} else if !hasDailyHooks {
			// For non-daily strategies, record equity on every tick.
			recordEquity(e.portfolio.Equity(), &equityCurve, &peakEquity, &maxDrawdown)
		}

		// --- Ask the strategy what to do ---
		orders := e.strategy.OnBar(baseTimeframe, tick, e.portfolio)
		if len(orders) > 0 {
			// Find next bar for fill simulation.
			nextIdx := nextSameSymbol[i]
			if nextIdx != -1 {
				nextBar := ticks[nextIdx]
				tickPrices := map[string]float64{tick.Symbol: nextBar.Open}
				trades = append(trades, e.fillOrders(orders, tickPrices, nextBar.Timestamp)...)
			}
		}

		// --- Feed through aggregators for higher-timeframe bars ---
		if hasMTF {
			for _, agg := range aggregators {
				agg.Update(tick)
			}
			for _, cb := range completedBars {
				barOrders := e.strategy.OnBar(cb.timeframe, cb.tick, e.portfolio)
				if len(barOrders) > 0 {
					nextIdx := nextSameSymbol[i]
					if nextIdx != -1 {
						nextBar := ticks[nextIdx]
						tickPrices := map[string]float64{tick.Symbol: nextBar.Open}
						trades = append(trades, e.fillOrders(barOrders, tickPrices, nextBar.Timestamp)...)
					}
				}
			}
			completedBars = completedBars[:0]
		}
	}

	// Fire final market close if the strategy uses daily hooks.
	if hasDailyHooks && currentDate != "" {
		// Snapshot end-of-last-day equity.
		recordEquity(e.portfolio.Equity(), &equityCurve, &peakEquity, &maxDrawdown)

		lastDay := days[dayIndex]
		closeOrders := dsh.OnMarketClose(e.portfolio)
		if len(closeOrders) > 0 {
			lastTick := ticks[len(ticks)-1]
			trades = append(trades, e.fillOrders(closeOrders, lastDay.closes, lastTick.Timestamp)...)
		}
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
