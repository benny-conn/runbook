package strategies

import (
	"fmt"

	"github.com/benny-conn/brandon-bot/risk"
	"github.com/benny-conn/brandon-bot/strategy"
)

// sma tracks a simple moving average using a circular buffer. O(1) per update.
type sma struct {
	buf   []float64
	pos   int
	count int
	sum   float64
}

func newSMA(period int) *sma {
	return &sma{buf: make([]float64, period)}
}

func (s *sma) update(price float64) float64 {
	s.sum -= s.buf[s.pos]
	s.buf[s.pos] = price
	s.sum += price
	s.pos = (s.pos + 1) % len(s.buf)
	if s.count < len(s.buf) {
		s.count++
	}
	return s.sum / float64(s.count)
}

func (s *sma) ready() bool { return s.count >= len(s.buf) }

// rsi tracks a 14-period RSI using Wilder's smoothing method.
type rsi struct {
	period    int
	count     int
	prevClose float64
	sumGain   float64 // accumulates during seed phase
	sumLoss   float64
	avgGain   float64
	avgLoss   float64
}

func newRSI(period int) *rsi { return &rsi{period: period} }

func (r *rsi) update(price float64) float64 {
	if r.count == 0 {
		r.prevClose = price
		r.count++
		return 50
	}

	change := price - r.prevClose
	r.prevClose = price
	r.count++

	gain, loss := 0.0, 0.0
	if change > 0 {
		gain = change
	} else {
		loss = -change
	}

	if r.count <= r.period {
		// Seed phase: accumulate simple averages.
		r.sumGain += gain
		r.sumLoss += loss
		if r.count == r.period {
			r.avgGain = r.sumGain / float64(r.period)
			r.avgLoss = r.sumLoss / float64(r.period)
		}
		return 50
	}

	// Wilder's smoothing.
	r.avgGain = (r.avgGain*float64(r.period-1) + gain) / float64(r.period)
	r.avgLoss = (r.avgLoss*float64(r.period-1) + loss) / float64(r.period)

	if r.avgLoss == 0 {
		return 100
	}
	return 100 - (100 / (1 + r.avgGain/r.avgLoss))
}

func (r *rsi) ready() bool { return r.count >= r.period }

// rsiSymbolState holds per-symbol indicator and position state.
type rsiSymbolState struct {
	ma200      *sma
	rsi        *rsi
	inPosition bool
	entryPrice float64
	qty        float64
}

// RSIPullback trades pullbacks within an uptrend:
//   - Trend filter: price must be above the 200-period SMA
//   - Entry: RSI(14) drops to or below 35 (oversold pullback)
//   - Exit: RSI(14) rises to or above 65, or 2% stop loss
//   - Position size: 10% of available cash per trade
type RSIPullback struct {
	symbols map[string]*rsiSymbolState
}

func NewRSIPullback() *RSIPullback {
	return &RSIPullback{symbols: make(map[string]*rsiSymbolState)}
}

func (s *RSIPullback) Name() string              { return "rsi_pullback" }
func (s *RSIPullback) Timeframes() []string       { return []string{"1m"} }

func (s *RSIPullback) getOrCreate(symbol string) *rsiSymbolState {
	if st, ok := s.symbols[symbol]; ok {
		return st
	}
	st := &rsiSymbolState{
		ma200: newSMA(200),
		rsi:   newRSI(14),
	}
	s.symbols[symbol] = st
	return st
}

func (s *RSIPullback) OnBar(_ string, tick strategy.Tick, portfolio strategy.Portfolio) []strategy.Order {
	st := s.getOrCreate(tick.Symbol)

	ma := st.ma200.update(tick.Close)
	rsiVal := st.rsi.update(tick.Close)

	if !st.ma200.ready() || !st.rsi.ready() {
		return nil
	}

	// Stop loss: 2% below entry.
	if st.inPosition && tick.Close <= st.entryPrice*0.98 {
		return []strategy.Order{{
			Symbol:    tick.Symbol,
			Side:      "sell",
			Qty:       st.qty,
			OrderType: "market",
			Reason:    "stop loss",
		}}
	}

	// Exit: RSI overbought.
	if st.inPosition && rsiVal >= 65 {
		return []strategy.Order{{
			Symbol:    tick.Symbol,
			Side:      "sell",
			Qty:       st.qty,
			OrderType: "market",
			Reason:    fmt.Sprintf("RSI overbought exit (RSI=%.1f)", rsiVal),
		}}
	}

	// Entry: price above 200 SMA (uptrend) and RSI oversold.
	if !st.inPosition && tick.Close > ma && rsiVal <= 35 {
		dollars := risk.PositionSize(portfolio.Cash(), 0.10)
		qty := risk.QtyForDollarAmount(dollars, tick.Close)
		if qty < 1 {
			return nil
		}
		return []strategy.Order{{
			Symbol:    tick.Symbol,
			Side:      "buy",
			Qty:       qty,
			OrderType: "market",
			Reason:    fmt.Sprintf("RSI pullback in uptrend (RSI=%.1f, price=%.2f, MA200=%.2f)", rsiVal, tick.Close, ma),
		}}
	}

	return nil
}

func (s *RSIPullback) OnFill(fill strategy.Fill) {
	st := s.getOrCreate(fill.Symbol)
	switch fill.Side {
	case "buy":
		st.inPosition = true
		st.entryPrice = fill.Price
		st.qty = fill.Qty
	case "sell":
		st.inPosition = false
		st.entryPrice = 0
		st.qty = 0
	}
}

// SeedPosition implements engine.PositionSeeder for safe restarts.
func (s *RSIPullback) SeedPosition(symbol string, qty, avgCost float64) {
	st := s.getOrCreate(symbol)
	st.inPosition = true
	st.entryPrice = avgCost
	st.qty = qty
}
