package strategies

import (
	"github.com/benny-conn/brandon-bot/risk"
	"github.com/benny-conn/brandon-bot/strategy"
)

// ema tracks an exponential moving average for a single price series.
type ema struct {
	period int
	k      float64
	count  int
	sum    float64 // accumulates until we can seed with SMA
	value  float64
}

func newEMA(period int) *ema {
	return &ema{
		period: period,
		k:      2.0 / float64(period+1),
	}
}

func (e *ema) update(price float64) {
	e.count++
	if e.count < e.period {
		e.sum += price
		return
	}
	if e.count == e.period {
		e.sum += price
		e.value = e.sum / float64(e.period) // seed with SMA
		return
	}
	e.value = price*e.k + e.value*(1-e.k)
}

func (e *ema) ready() bool { return e.count >= e.period }

// maSymbolState holds per-symbol EMA state and open position tracking.
type maSymbolState struct {
	fast       *ema
	slow       *ema
	prevFast   float64
	prevSlow   float64
	prevReady  bool // true once we have at least one prior tick with both EMAs ready
	inPosition bool
	entryPrice float64
	qty        float64
}

// MACrossover implements a 9/21 EMA crossover strategy:
//   - Buy when 9-EMA crosses above 21-EMA (10% of cash, market order)
//   - Sell when 9-EMA crosses below 21-EMA, or price drops 2% below entry (stop loss)
type MACrossover struct {
	symbols map[string]*maSymbolState
}

func NewMACrossover() *MACrossover {
	return &MACrossover{symbols: make(map[string]*maSymbolState)}
}

func (m *MACrossover) Name() string { return "ma_crossover" }

func (m *MACrossover) getOrCreate(symbol string) *maSymbolState {
	if s, ok := m.symbols[symbol]; ok {
		return s
	}
	s := &maSymbolState{fast: newEMA(9), slow: newEMA(21)}
	m.symbols[symbol] = s
	return s
}

func (m *MACrossover) OnTick(tick strategy.Tick, portfolio strategy.Portfolio) []strategy.Order {
	s := m.getOrCreate(tick.Symbol)

	s.fast.update(tick.Close)
	s.slow.update(tick.Close)

	if !s.fast.ready() || !s.slow.ready() {
		return nil
	}

	currFast := s.fast.value
	currSlow := s.slow.value

	// Stop loss: exit immediately if price is 2% below entry.
	if s.inPosition && tick.Close <= s.entryPrice*0.98 {
		return []strategy.Order{{
			Symbol:    tick.Symbol,
			Side:      "sell",
			Qty:       s.qty,
			OrderType: "market",
			Reason:    "stop loss triggered",
		}}
	}

	var orders []strategy.Order

	if s.prevReady {
		bullCross := s.prevFast <= s.prevSlow && currFast > currSlow
		bearCross := s.prevFast >= s.prevSlow && currFast < currSlow

		if bullCross && !s.inPosition {
			dollars := risk.PositionSize(portfolio.Cash(), 0.10)
			qty := risk.QtyForDollarAmount(dollars, tick.Close)
			if qty >= 1 {
				orders = append(orders, strategy.Order{
					Symbol:    tick.Symbol,
					Side:      "buy",
					Qty:       qty,
					OrderType: "market",
					Reason:    "9 EMA crossed above 21 EMA",
				})
			}
		} else if bearCross && s.inPosition {
			orders = append(orders, strategy.Order{
				Symbol:    tick.Symbol,
				Side:      "sell",
				Qty:       s.qty,
				OrderType: "market",
				Reason:    "9 EMA crossed below 21 EMA",
			})
		}
	}

	s.prevFast = currFast
	s.prevSlow = currSlow
	s.prevReady = true

	return orders
}

// SeedPosition implements engine.PositionSeeder.
// Called on startup if the bot restarts while holding a position,
// so the strategy knows it's already in a trade and at what cost.
func (m *MACrossover) SeedPosition(symbol string, qty, avgCost float64) {
	s := m.getOrCreate(symbol)
	s.inPosition = true
	s.entryPrice = avgCost
	s.qty = qty
}

func (m *MACrossover) OnFill(fill strategy.Fill) {
	s := m.getOrCreate(fill.Symbol)
	switch fill.Side {
	case "buy":
		s.inPosition = true
		s.entryPrice = fill.Price
		s.qty = fill.Qty
	case "sell":
		s.inPosition = false
		s.entryPrice = 0
		s.qty = 0
	}
}
