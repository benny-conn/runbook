package portfolio

import (
	"sync"

	"github.com/benny-conn/brandon-bot/strategy"
)

// SimulatedPortfolio tracks cash, positions, and P&L for backtesting.
// It is safe for concurrent use.
//
// When multipliers are set (via SetMultipliers), symbols with a multiplier > 1
// are treated as futures: P&L is scaled by the multiplier, and cash only changes
// by realized P&L (no notional value exchange). Symbols without a multiplier
// (or multiplier == 1) behave as equities.
type SimulatedPortfolio struct {
	mu          sync.RWMutex
	cash        float64
	realizedPL  float64
	positions   map[string]*strategy.Position
	multipliers map[string]float64 // symbol → point value (1.0 for equities)
}

func NewSimulatedPortfolio(initialCash float64) *SimulatedPortfolio {
	return &SimulatedPortfolio{
		cash:      initialCash,
		positions: make(map[string]*strategy.Position),
	}
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
	return &cp
}

func (p *SimulatedPortfolio) Positions() []strategy.Position {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]strategy.Position, 0, len(p.positions))
	for _, pos := range p.positions {
		result = append(result, *pos)
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

// ClassifyFillSide returns the effective side for a fill: "buy", "sell", or "short".
// "short" indicates the sell is opening a new short rather than closing a long.
// Must be called BEFORE ApplyFill.
func (p *SimulatedPortfolio) ClassifyFillSide(fill strategy.Fill) string {
	if fill.Side != "sell" {
		return fill.Side
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	pos, exists := p.positions[fill.Symbol]
	if !exists || pos.Qty <= 0 {
		return "short" // no long position — this sell opens a short
	}
	if fill.Qty > pos.Qty {
		// Partially closes long, partially opens short — call it "sell" for now;
		// the closing portion is captured in ComputeFillPL.
		return "sell"
	}
	return "sell"
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
		} else if pos.Qty < 0 {
			// Covering a short position — realize P&L on the covered portion.
			coveredQty := fill.Qty
			if coveredQty > -pos.Qty {
				coveredQty = -pos.Qty
			}
			realizedPL := (pos.AvgCost - fill.Price) * coveredQty * mult
			p.realizedPL += realizedPL
			if futures {
				p.cash += realizedPL
			}

			pos.Qty += fill.Qty
			if pos.Qty == 0 {
				delete(p.positions, fill.Symbol)
			} else if pos.Qty > 0 {
				// Buy exceeded short qty — flipped to long. Reset avg cost.
				pos.AvgCost = fill.Price
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
		} else if pos.Qty < 0 && wasLong {
			// Sell exceeded long qty — flipped to short. Reset avg cost.
			pos.AvgCost = fill.Price
		}
	}
}

// UpdateMarketPrice refreshes the market value and unrealized P&L for a symbol.
// Called on every tick so equity stays current.
func (p *SimulatedPortfolio) UpdateMarketPrice(symbol string, price float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
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
