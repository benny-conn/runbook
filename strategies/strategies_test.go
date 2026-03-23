package strategies

import (
	"math"
	"testing"

	"github.com/benny-conn/brandon-bot/strategy"
)

func approxEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

// --- EMA tests ---

func TestEMA_SeedWithSMA(t *testing.T) {
	e := newEMA(3)
	e.update(2)
	e.update(4)
	if e.ready() {
		t.Error("should not be ready after 2 values for period 3")
	}
	e.update(6)
	if !e.ready() {
		t.Error("should be ready after 3 values")
	}
	// SMA seed = (2+4+6)/3 = 4
	if !approxEqual(e.value, 4, 0.001) {
		t.Errorf("seed value = %v, want 4", e.value)
	}
}

func TestEMA_ExponentialSmoothing(t *testing.T) {
	e := newEMA(3)
	// k = 2/(3+1) = 0.5
	e.update(2)
	e.update(4)
	e.update(6) // seed = 4.0
	e.update(8) // EMA = 8*0.5 + 4*0.5 = 6.0
	if !approxEqual(e.value, 6.0, 0.001) {
		t.Errorf("EMA = %v, want 6.0", e.value)
	}
	e.update(10) // EMA = 10*0.5 + 6*0.5 = 8.0
	if !approxEqual(e.value, 8.0, 0.001) {
		t.Errorf("EMA = %v, want 8.0", e.value)
	}
}

// --- SMA tests ---

func TestSMA_Basic(t *testing.T) {
	s := newSMA(3)
	s.update(10)
	s.update(20)
	val := s.update(30)
	// (10+20+30)/3 = 20
	if !approxEqual(val, 20, 0.001) {
		t.Errorf("SMA = %v, want 20", val)
	}
	if !s.ready() {
		t.Error("should be ready after 3 values")
	}
}

func TestSMA_CircularBuffer(t *testing.T) {
	s := newSMA(3)
	s.update(10)
	s.update(20)
	s.update(30) // [10,20,30], avg=20
	val := s.update(40) // [40,20,30], avg=30
	if !approxEqual(val, 30, 0.001) {
		t.Errorf("SMA = %v, want 30", val)
	}
	val = s.update(50) // [40,50,30], avg=40
	if !approxEqual(val, 40, 0.001) {
		t.Errorf("SMA = %v, want 40 after wrap", val)
	}
}

func TestSMA_NotReady(t *testing.T) {
	s := newSMA(5)
	s.update(10)
	s.update(20)
	if s.ready() {
		t.Error("should not be ready after 2 values for period 5")
	}
}

// --- RSI tests ---

func TestRSI_SeedPhase(t *testing.T) {
	r := newRSI(3)
	val := r.update(100) // count=1, first price
	if val != 50 {
		t.Errorf("first RSI = %v, want 50", val)
	}
	val = r.update(101) // count=2, seed phase
	if val != 50 {
		t.Errorf("seed phase RSI = %v, want 50", val)
	}
}

func TestRSI_Ready(t *testing.T) {
	r := newRSI(3)
	for i := 0; i < 2; i++ {
		r.update(float64(100 + i))
	}
	if r.ready() {
		t.Error("should not be ready before period values")
	}
	r.update(102)
	if !r.ready() {
		t.Error("should be ready at period count")
	}
}

func TestRSI_AllGains(t *testing.T) {
	r := newRSI(3)
	// Monotonically increasing prices
	r.update(100)
	r.update(101)
	r.update(102) // seed: avgGain = 1, avgLoss = 0
	val := r.update(103)
	// After seed, avgLoss = 0, so RSI = 100
	if val != 100 {
		t.Errorf("all-gains RSI = %v, want 100", val)
	}
}

func TestRSI_MixedPrices(t *testing.T) {
	r := newRSI(14)
	// Feed enough data to get past seed phase
	prices := []float64{
		44, 44.34, 44.09, 43.61, 44.33, 44.83, 45.10,
		45.42, 45.84, 46.08, 45.89, 46.03, 45.61, 46.28, 46.28,
	}
	var val float64
	for _, p := range prices {
		val = r.update(p)
	}
	if !r.ready() {
		t.Fatal("RSI should be ready after 15 prices")
	}
	// RSI should be between 0 and 100
	if val < 0 || val > 100 {
		t.Errorf("RSI = %v, should be 0-100", val)
	}
}

// --- MACrossover tests ---

func TestMACrossover_Name(t *testing.T) {
	m := NewMACrossover()
	if m.Name() != "ma_crossover" {
		t.Errorf("Name() = %q, want %q", m.Name(), "ma_crossover")
	}
}

// mockPortfolio implements strategy.Portfolio for testing.
type mockPortfolio struct {
	cash      float64
	positions map[string]*strategy.Position
}

func newMockPortfolio(cash float64) *mockPortfolio {
	return &mockPortfolio{cash: cash, positions: make(map[string]*strategy.Position)}
}

func (p *mockPortfolio) Cash() float64                    { return p.cash }
func (p *mockPortfolio) Equity() float64                  { return p.cash }
func (p *mockPortfolio) Position(s string) *strategy.Position { return p.positions[s] }
func (p *mockPortfolio) Positions() []strategy.Position       { return nil }
func (p *mockPortfolio) TotalPL() float64                 { return 0 }

func TestMACrossover_NeedsWarmup(t *testing.T) {
	m := NewMACrossover()
	p := newMockPortfolio(10000)

	// First 20 ticks should produce no orders (need 21 for slow EMA)
	for i := 0; i < 20; i++ {
		orders := m.OnTick(strategy.Tick{Symbol: "AAPL", Close: 100 + float64(i)}, p)
		if len(orders) > 0 {
			t.Errorf("tick %d: expected no orders during warmup, got %d", i, len(orders))
		}
	}
}

func TestMACrossover_BullishCrossover(t *testing.T) {
	m := NewMACrossover()
	p := newMockPortfolio(100000)

	// Feed 21 ticks of flat data to warm up both EMAs
	for i := 0; i < 21; i++ {
		m.OnTick(strategy.Tick{Symbol: "AAPL", Close: 100}, p)
	}

	// Now feed rising prices to make fast EMA cross above slow EMA
	var lastOrders []strategy.Order
	for i := 0; i < 20; i++ {
		lastOrders = m.OnTick(strategy.Tick{Symbol: "AAPL", Close: 100 + float64(i)*5}, p)
		if len(lastOrders) > 0 {
			break
		}
	}

	if len(lastOrders) == 0 {
		t.Fatal("expected a buy order on bullish crossover")
	}
	if lastOrders[0].Side != "buy" {
		t.Errorf("expected buy order, got %q", lastOrders[0].Side)
	}
}

func TestMACrossover_StopLoss(t *testing.T) {
	m := NewMACrossover()
	p := newMockPortfolio(100000)

	// Warm up both EMAs (need 21 ticks for slow EMA)
	for i := 0; i < 21; i++ {
		m.OnTick(strategy.Tick{Symbol: "AAPL", Close: 100}, p)
	}

	// Simulate being in a position at entry price 100
	m.SeedPosition("AAPL", 10, 100)

	// Price drops 3% below entry (below 2% threshold)
	orders := m.OnTick(strategy.Tick{Symbol: "AAPL", Close: 97}, p)
	if len(orders) != 1 || orders[0].Side != "sell" {
		t.Errorf("expected stop loss sell order, got %v", orders)
	}
}

func TestMACrossover_OnFill(t *testing.T) {
	m := NewMACrossover()
	m.OnFill(strategy.Fill{Symbol: "AAPL", Side: "buy", Qty: 10, Price: 100})

	s := m.symbols["AAPL"]
	if !s.inPosition || s.entryPrice != 100 || s.qty != 10 {
		t.Error("OnFill buy did not update state correctly")
	}

	m.OnFill(strategy.Fill{Symbol: "AAPL", Side: "sell", Qty: 10, Price: 110})
	if s.inPosition || s.entryPrice != 0 || s.qty != 0 {
		t.Error("OnFill sell did not clear state correctly")
	}
}

func TestMACrossover_SeedPosition(t *testing.T) {
	m := NewMACrossover()
	m.SeedPosition("AAPL", 10, 150)

	s := m.symbols["AAPL"]
	if !s.inPosition || s.entryPrice != 150 || s.qty != 10 {
		t.Error("SeedPosition did not set state correctly")
	}
}

func TestMACrossover_MultiSymbol(t *testing.T) {
	m := NewMACrossover()
	p := newMockPortfolio(100000)

	// Feed ticks for two symbols
	for i := 0; i < 25; i++ {
		m.OnTick(strategy.Tick{Symbol: "AAPL", Close: 100}, p)
		m.OnTick(strategy.Tick{Symbol: "GOOG", Close: 200}, p)
	}

	if len(m.symbols) != 2 {
		t.Errorf("expected 2 symbol states, got %d", len(m.symbols))
	}
	if m.symbols["AAPL"].fast.ready() == false || m.symbols["GOOG"].fast.ready() == false {
		t.Error("both symbols should have warm EMAs")
	}
}

// --- RSIPullback tests ---

func TestRSIPullback_Name(t *testing.T) {
	s := NewRSIPullback()
	if s.Name() != "rsi_pullback" {
		t.Errorf("Name() = %q", s.Name())
	}
}

func TestRSIPullback_NeedsWarmup(t *testing.T) {
	s := NewRSIPullback()
	p := newMockPortfolio(100000)

	// Need 200 ticks for SMA warmup
	for i := 0; i < 200; i++ {
		orders := s.OnTick(strategy.Tick{Symbol: "AAPL", Close: 100 + float64(i)*0.01}, p)
		if len(orders) > 0 {
			t.Errorf("tick %d: expected no orders during warmup", i)
		}
	}
}

func TestRSIPullback_StopLoss(t *testing.T) {
	s := NewRSIPullback()
	p := newMockPortfolio(100000)

	s.SeedPosition("AAPL", 10, 100)

	// Feed enough data to warm up indicators
	for i := 0; i < 200; i++ {
		s.OnTick(strategy.Tick{Symbol: "AAPL", Close: 100}, p)
	}

	// Price drops 3% below entry
	orders := s.OnTick(strategy.Tick{Symbol: "AAPL", Close: 97}, p)
	if len(orders) != 1 || orders[0].Side != "sell" {
		t.Errorf("expected stop loss sell, got %v", orders)
	}
}

func TestRSIPullback_OnFill(t *testing.T) {
	s := NewRSIPullback()
	s.OnFill(strategy.Fill{Symbol: "AAPL", Side: "buy", Qty: 10, Price: 100})

	st := s.symbols["AAPL"]
	if !st.inPosition || st.entryPrice != 100 || st.qty != 10 {
		t.Error("OnFill buy did not update state correctly")
	}

	s.OnFill(strategy.Fill{Symbol: "AAPL", Side: "sell", Qty: 10, Price: 110})
	if st.inPosition || st.entryPrice != 0 || st.qty != 0 {
		t.Error("OnFill sell did not clear state correctly")
	}
}

func TestRSIPullback_SeedPosition(t *testing.T) {
	s := NewRSIPullback()
	s.SeedPosition("AAPL", 10, 150)

	st := s.symbols["AAPL"]
	if !st.inPosition || st.entryPrice != 150 || st.qty != 10 {
		t.Error("SeedPosition did not set state correctly")
	}
}
