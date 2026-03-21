package backtest

import (
	"fmt"
	"math"

	"brandon-bot/internal/portfolio"
	"brandon-bot/strategy"
)

// Trade records a completed fill along with realized P&L (for sells).
type Trade struct {
	Fill       strategy.Fill
	RealizedPL float64 // non-zero only for sell fills
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

// Engine runs a strategy against a sorted slice of historical ticks.
type Engine struct {
	strategy  strategy.Strategy
	portfolio *portfolio.SimulatedPortfolio
}

func NewEngine(strat strategy.Strategy, initialCapital float64) *Engine {
	return &Engine{
		strategy:  strat,
		portfolio: portfolio.NewSimulatedPortfolio(initialCapital),
	}
}

// Run replays ticks chronologically, simulates fills at the next bar's open,
// and returns full performance metrics.
func (e *Engine) Run(ticks []strategy.Tick) *Results {
	if len(ticks) == 0 {
		return &Results{InitialCapital: e.portfolio.Cash()}
	}

	initialCapital := e.portfolio.Cash()

	// Precompute: for each tick index, the index of the next tick for the same symbol.
	// Used to simulate fills at next bar's open.
	nextSameSymbol := precomputeNextSameSymbol(ticks)

	var (
		trades      []Trade
		equityCurve []float64
		peakEquity  float64
		maxDrawdown float64
	)

	for i, tick := range ticks {
		// Keep market values current so Equity() is accurate.
		e.portfolio.UpdateMarketPrice(tick.Symbol, tick.Close)

		eq := e.portfolio.Equity()
		equityCurve = append(equityCurve, eq)

		if eq > peakEquity {
			peakEquity = eq
		}
		if peakEquity > 0 {
			dd := (peakEquity - eq) / peakEquity * 100
			if dd > maxDrawdown {
				maxDrawdown = dd
			}
		}

		// Ask the strategy what to do.
		orders := e.strategy.OnTick(tick, e.portfolio)
		if len(orders) == 0 {
			continue
		}

		// Find next bar for fill simulation.
		nextIdx := nextSameSymbol[i]
		if nextIdx == -1 {
			// Last bar for this symbol — can't fill, drop the orders.
			continue
		}
		nextBar := ticks[nextIdx]

		for _, order := range orders {
			fillPrice := nextBar.Open
			fillTime := nextBar.Timestamp

			// Basic cash check for buys.
			if order.Side == "buy" {
				cost := order.Qty * fillPrice
				if cost > e.portfolio.Cash() {
					continue
				}
			}

			// For sells, capture avg cost before the fill removes the position.
			var realizedPL float64
			if order.Side == "sell" {
				if pos := e.portfolio.Position(order.Symbol); pos != nil {
					realizedPL = (fillPrice - pos.AvgCost) * order.Qty
				}
			}

			fill := strategy.Fill{
				Symbol:    order.Symbol,
				Side:      order.Side,
				Qty:       order.Qty,
				Price:     fillPrice,
				Timestamp: fillTime,
			}

			e.portfolio.ApplyFill(fill)
			e.strategy.OnFill(fill)

			trades = append(trades, Trade{Fill: fill, RealizedPL: realizedPL})
		}
	}

	finalEquity := e.portfolio.Equity()

	wins, losses := 0, 0
	for _, t := range trades {
		if t.Fill.Side == "sell" {
			if t.RealizedPL > 0 {
				wins++
			} else {
				losses++
			}
		}
	}

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
