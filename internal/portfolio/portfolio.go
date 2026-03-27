package portfolio

import (
	"sync"

	"github.com/benny-conn/runbook/strategy"
)

// SimulatedPortfolio tracks cash, positions, and P&L for backtesting.
// It is safe for concurrent use.
//
// When multipliers are set (via SetMultipliers), symbols with a multiplier > 1
// are treated as futures: P&L is scaled by the multiplier, and cash only changes
// by realized P&L (no notional value exchange). Symbols without a multiplier
// (or multiplier == 1) behave as equities.
//
// NOTE: All financial values use float64 arithmetic. For typical trading
// scenarios (sub-million dollar accounts, standard lot sizes) this provides
// ~15 digits of precision which is more than sufficient. Rounding errors
// may accumulate over thousands of high-frequency fills but remain negligible
// for practical purposes.
type SimulatedPortfolio struct {
	mu          sync.RWMutex
	cash        float64
	realizedPL  float64
	positions   map[string]*strategy.Position
	multipliers map[string]float64 // symbol → point value (1.0 for equities)
	holdingBars map[string]int     // symbol → bars since position opened
	lastPrices  map[string]float64 // symbol → most recent market price

	// Daily tracking — reset by ResetDaily().
	dailyRealizedPL float64 // realized P&L since last daily reset
	dailyTrades     int     // completed round-trips since last daily reset
}

func NewSimulatedPortfolio(initialCash float64) *SimulatedPortfolio {
	return &SimulatedPortfolio{
		cash:        initialCash,
		positions:   make(map[string]*strategy.Position),
		holdingBars: make(map[string]int),
		lastPrices:  make(map[string]float64),
	}
}

// ResetDaily clears daily P&L and trade counters. Called by the engine at market open.
func (p *SimulatedPortfolio) ResetDaily() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dailyRealizedPL = 0
	p.dailyTrades = 0
}

// DailyPL returns realized + unrealized P&L since the last daily reset.
func (p *SimulatedPortfolio) DailyPL() float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	upl := 0.0
	for _, pos := range p.positions {
		upl += pos.UnrealizedPL
	}
	return p.dailyRealizedPL + upl
}

// DailyTrades returns the number of completed round-trips since the last daily reset.
func (p *SimulatedPortfolio) DailyTrades() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.dailyTrades
}

// SetMultipliers configures per-symbol contract multipliers for futures P&L.
// For equities, multiplier is 1.0 (default when absent).
// For MNQ it's 2.0, ES is 50.0, MES is 5.0, etc.
func (p *SimulatedPortfolio) SetMultipliers(m map[string]float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.multipliers = m
}

// multiplier returns the point value multiplier for a symbol.
// Returns 1.0 if no multiplier is set (equity behavior).
func (p *SimulatedPortfolio) multiplier(symbol string) float64 {
	if p.multipliers == nil {
		return 1.0
	}
	if m, ok := p.multipliers[symbol]; ok && m > 0 {
		return m
	}
	return 1.0
}

// isFutures returns true if the symbol has a multiplier > 1 (futures behavior).
func (p *SimulatedPortfolio) isFutures(symbol string) bool {
	return p.multiplier(symbol) > 1.0
}

// SetRealizedPL seeds the accumulated realized P&L. Used on recovery to carry
// forward P&L from prior engine runs so the display stays accurate across restarts.
func (p *SimulatedPortfolio) SetRealizedPL(pl float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.realizedPL = pl
}

func (p *SimulatedPortfolio) Cash() float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cash
}

func (p *SimulatedPortfolio) Equity() float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	total := p.cash
	for _, pos := range p.positions {
		if p.isFutures(pos.Symbol) {
			// For futures, equity contribution is unrealized P&L (not notional value).
			total += pos.UnrealizedPL
		} else {
			total += pos.MarketValue
		}
	}
	return total
}

func (p *SimulatedPortfolio) Position(symbol string) *strategy.Position {
	p.mu.RLock()
	defer p.mu.RUnlock()
	pos, ok := p.positions[symbol]
	if !ok {
		return nil
	}
	// Return a copy so callers can't mutate internal state.
	cp := *pos
	cp.EntryPrice = cp.AvgCost
	cp.HoldingBars = p.holdingBars[symbol]
	if cp.Qty > 0 {
		cp.Side = "long"
	} else if cp.Qty < 0 {
		cp.Side = "short"
	} else {
		cp.Side = "flat"
	}
	return &cp
}

func (p *SimulatedPortfolio) Positions() []strategy.Position {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]strategy.Position, 0, len(p.positions))
	for _, pos := range p.positions {
		cp := *pos
		cp.EntryPrice = cp.AvgCost
		cp.HoldingBars = p.holdingBars[cp.Symbol]
		if cp.Qty > 0 {
			cp.Side = "long"
		} else if cp.Qty < 0 {
			cp.Side = "short"
		} else {
			cp.Side = "flat"
		}
		result = append(result, cp)
	}
	return result
}

func (p *SimulatedPortfolio) TotalPL() float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	total := p.realizedPL
	for _, pos := range p.positions {
		total += pos.UnrealizedPL
	}
	return total
}

// IncrementHoldingBars increments the bar counter for a symbol's open position.
func (p *SimulatedPortfolio) IncrementHoldingBars(symbol string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.positions[symbol]; ok {
		p.holdingBars[symbol]++
	}
}

// ComputeFillPL returns the realized P&L for a fill based on the current position.
// Must be called BEFORE ApplyFill so the position state is still intact.
func (p *SimulatedPortfolio) ComputeFillPL(fill strategy.Fill) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()

	pos, exists := p.positions[fill.Symbol]
	if !exists {
		return 0 // opening a new position — no realized P&L
	}

	mult := p.multiplier(fill.Symbol)

	switch fill.Side {
	case "sell":
		if pos.Qty > 0 {
			// Closing (or reducing) a long position.
			closedQty := fill.Qty
			if closedQty > pos.Qty {
				closedQty = pos.Qty // only the portion that closes the long
			}
			return (fill.Price - pos.AvgCost) * closedQty * mult
		}
	case "buy":
		if pos.Qty < 0 {
			// Covering (or reducing) a short position.
			closedQty := fill.Qty
			if closedQty > -pos.Qty {
				closedQty = -pos.Qty // only the portion that covers the short
			}
			return (pos.AvgCost - fill.Price) * closedQty * mult
		}
	}
	return 0
}

// ClassifyFillSide returns the effective side for a fill: always "buy" or "sell".
// Must be called BEFORE ApplyFill.
func (p *SimulatedPortfolio) ClassifyFillSide(fill strategy.Fill) string {
	if fill.Side == "short" {
		return "sell" // normalize "short" to "sell"
	}
	return fill.Side
}

// ApplyFill updates cash and positions based on a completed fill.
// For futures (multiplier > 1), cash only changes by realized P&L — no notional
// value is exchanged. For equities (multiplier == 1), cash changes by qty × price.
func (p *SimulatedPortfolio) ApplyFill(fill strategy.Fill) {
	p.mu.Lock()
	defer p.mu.Unlock()

	mult := p.multiplier(fill.Symbol)
	futures := mult > 1.0

	switch fill.Side {
	case "buy":
		if !futures {
			p.cash -= fill.Qty * fill.Price
		}
		pos, exists := p.positions[fill.Symbol]
		if !exists {
			p.positions[fill.Symbol] = &strategy.Position{
				Symbol:  fill.Symbol,
				Qty:     fill.Qty,
				AvgCost: fill.Price,
			}
			p.holdingBars[fill.Symbol] = 0
		} else if pos.Qty < 0 {
			// Covering a short position — realize P&L on the covered portion.
			coveredQty := fill.Qty
			if coveredQty > -pos.Qty {
				coveredQty = -pos.Qty
			}
			realizedPL := (pos.AvgCost - fill.Price) * coveredQty * mult
			p.realizedPL += realizedPL
			p.dailyRealizedPL += realizedPL
			if futures {
				p.cash += realizedPL
			}

			pos.Qty += fill.Qty
			if pos.Qty == 0 {
				delete(p.positions, fill.Symbol)
				delete(p.holdingBars, fill.Symbol)
				p.dailyTrades++
			} else if pos.Qty > 0 {
				// Buy exceeded short qty — flipped to long. Reset avg cost.
				pos.AvgCost = fill.Price
				p.holdingBars[fill.Symbol] = 0
				if !futures {
					// For equities, the excess buy cost is already deducted above.
				}
			}
		} else {
			// Adding to a long position.
			totalCost := pos.Qty*pos.AvgCost + fill.Qty*fill.Price
			pos.Qty += fill.Qty
			pos.AvgCost = totalCost / pos.Qty
		}
	case "sell", "short":
		pos, exists := p.positions[fill.Symbol]
		if !exists {
			// Opening a new short position (negative qty).
			if !futures {
				p.cash += fill.Qty * fill.Price
			}
			p.positions[fill.Symbol] = &strategy.Position{
				Symbol:  fill.Symbol,
				Qty:     -fill.Qty,
				AvgCost: fill.Price,
			}
			p.holdingBars[fill.Symbol] = 0
			return
		}
		wasLong := pos.Qty > 0
		if wasLong {
			// Closing (or reducing) a long position — realize P&L on the closed portion.
			closedQty := fill.Qty
			if closedQty > pos.Qty {
				closedQty = pos.Qty
			}
			realizedPL := (fill.Price - pos.AvgCost) * closedQty * mult
			p.realizedPL += realizedPL
			p.dailyRealizedPL += realizedPL
			if futures {
				p.cash += realizedPL
			}
		}
		if !futures {
			p.cash += fill.Qty * fill.Price
		}
		pos.Qty -= fill.Qty
		if pos.Qty == 0 {
			delete(p.positions, fill.Symbol)
			delete(p.holdingBars, fill.Symbol)
			p.dailyTrades++
		} else if pos.Qty < 0 && wasLong {
			// Sell exceeded long qty — flipped to short. Reset avg cost.
			pos.AvgCost = fill.Price
			p.holdingBars[fill.Symbol] = 0
		} else if pos.Qty < 0 && !wasLong {
			// Adding to an existing short position — average the cost basis.
			// pos.Qty has already been decremented, so the previous short qty
			// was -(pos.Qty + fill.Qty) and the new total is -pos.Qty.
			prevShortQty := -(pos.Qty + fill.Qty)
			totalCost := prevShortQty*pos.AvgCost + fill.Qty*fill.Price
			pos.AvgCost = totalCost / (-pos.Qty)
		}
	}
}

// LastPrice returns the most recent market price for a symbol (0 if never seen).
func (p *SimulatedPortfolio) LastPrice(symbol string) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastPrices[symbol]
}

// UpdateMarketPrice refreshes the market value and unrealized P&L for a symbol.
// Called on every tick so equity stays current.
func (p *SimulatedPortfolio) UpdateMarketPrice(symbol string, price float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastPrices[symbol] = price
	pos, exists := p.positions[symbol]
	if !exists {
		return
	}

	mult := p.multiplier(symbol)

	if pos.Qty > 0 {
		pos.UnrealizedPL = (price - pos.AvgCost) * pos.Qty * mult
		if mult > 1.0 {
			// For futures, market value IS the unrealized P&L (no notional).
			pos.MarketValue = pos.UnrealizedPL
		} else {
			pos.MarketValue = pos.Qty * price
		}
	} else {
		// Short position: P&L inverted.
		pos.UnrealizedPL = (pos.AvgCost - price) * (-pos.Qty) * mult
		if mult > 1.0 {
			pos.MarketValue = pos.UnrealizedPL
		} else {
			pos.MarketValue = pos.Qty * price // negative
		}
	}
}
