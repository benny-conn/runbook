package portfolio

import (
	"sync"

	"brandon-bot/strategy"
)

// SimulatedPortfolio tracks cash, positions, and P&L for backtesting.
// It is safe for concurrent use.
type SimulatedPortfolio struct {
	mu        sync.RWMutex
	cash      float64
	positions map[string]*strategy.Position
}

func NewSimulatedPortfolio(initialCash float64) *SimulatedPortfolio {
	return &SimulatedPortfolio{
		cash:      initialCash,
		positions: make(map[string]*strategy.Position),
	}
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
		total += pos.MarketValue
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
	var total float64
	for _, pos := range p.positions {
		total += pos.UnrealizedPL
	}
	return total
}

// ApplyFill updates cash and positions based on a completed fill.
func (p *SimulatedPortfolio) ApplyFill(fill strategy.Fill) {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch fill.Side {
	case "buy":
		p.cash -= fill.Qty * fill.Price
		if pos, exists := p.positions[fill.Symbol]; exists {
			totalCost := pos.Qty*pos.AvgCost + fill.Qty*fill.Price
			pos.Qty += fill.Qty
			pos.AvgCost = totalCost / pos.Qty
		} else {
			p.positions[fill.Symbol] = &strategy.Position{
				Symbol:  fill.Symbol,
				Qty:     fill.Qty,
				AvgCost: fill.Price,
			}
		}
	case "sell":
		p.cash += fill.Qty * fill.Price
		if pos, exists := p.positions[fill.Symbol]; exists {
			pos.Qty -= fill.Qty
			if pos.Qty <= 0 {
				delete(p.positions, fill.Symbol)
			}
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
	pos.MarketValue = pos.Qty * price
	pos.UnrealizedPL = (price - pos.AvgCost) * pos.Qty
}
