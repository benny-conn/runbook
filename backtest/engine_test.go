package backtest

import (
	"math"
	"testing"
	"time"

	"github.com/benny-conn/brandon-bot/strategy"
)

func approxEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

// --- Mock strategy for testing ---

type mockStrategy struct {
	name    string
	onTick  func(tick strategy.Tick, p strategy.Portfolio) []strategy.Order
	onFill  func(fill strategy.Fill)
	fills   []strategy.Fill
}

func (m *mockStrategy) Name() string { return m.name }
func (m *mockStrategy) OnTick(tick strategy.Tick, p strategy.Portfolio) []strategy.Order {
	if m.onTick != nil {
		return m.onTick(tick, p)
	}
	return nil
}
func (m *mockStrategy) OnFill(fill strategy.Fill) {
	m.fills = append(m.fills, fill)
	if m.onFill != nil {
		m.onFill(fill)
	}
}

// --- Helper to create ticks ---

func tick(symbol string, day int, open, close float64) strategy.Tick {
	return strategy.Tick{
		Symbol:    symbol,
		Timestamp: time.Date(2024, 1, day, 10, 0, 0, 0, time.UTC),
		Open:      open,
		High:      close + 1,
		Low:       open - 1,
		Close:     close,
	}
}

// --- Tests ---

func TestEngine_RunEmpty(t *testing.T) {
	e := NewEngine(&mockStrategy{name: "test"}, 10000)
	r := e.Run(nil)
	if r.InitialCapital != 10000 {
		t.Errorf("InitialCapital = %v, want 10000", r.InitialCapital)
	}
	if r.TotalTrades != 0 {
		t.Errorf("TotalTrades = %v, want 0", r.TotalTrades)
	}
}

func TestEngine_BuyAndSell(t *testing.T) {
	callCount := 0
	strat := &mockStrategy{
		name: "buy_sell",
		onTick: func(tick strategy.Tick, p strategy.Portfolio) []strategy.Order {
			callCount++
			if callCount == 2 {
				return []strategy.Order{{
					Symbol: "AAPL", Side: "buy", Qty: 10, OrderType: "market",
				}}
			}
			if callCount == 4 {
				return []strategy.Order{{
					Symbol: "AAPL", Side: "sell", Qty: 10, OrderType: "market",
				}}
			}
			return nil
		},
	}

	ticks := []strategy.Tick{
		tick("AAPL", 1, 100, 100),
		tick("AAPL", 2, 101, 102),
		tick("AAPL", 3, 103, 104),
		tick("AAPL", 4, 105, 106),
		tick("AAPL", 5, 107, 108),
	}

	e := NewEngine(strat, 10000)
	r := e.Run(ticks)

	// Buy order on tick 2 fills at tick 3 open (103)
	// Sell order on tick 4 fills at tick 5 open (107)
	// Realized P&L: (107-103)*10 = 40
	if r.TotalTrades != 1 {
		t.Errorf("TotalTrades = %v, want 1", r.TotalTrades)
	}
	if r.WinningTrades != 1 {
		t.Errorf("WinningTrades = %v, want 1", r.WinningTrades)
	}
	if r.LosingTrades != 0 {
		t.Errorf("LosingTrades = %v, want 0", r.LosingTrades)
	}

	// Final equity = 10000 - 10*103 + 10*107 = 10040
	if !approxEqual(r.FinalEquity, 10040, 0.01) {
		t.Errorf("FinalEquity = %v, want 10040", r.FinalEquity)
	}
}

func TestEngine_InsufficientCash(t *testing.T) {
	strat := &mockStrategy{
		name: "insufficient",
		onTick: func(tick strategy.Tick, p strategy.Portfolio) []strategy.Order {
			return []strategy.Order{{
				Symbol: "AAPL", Side: "buy", Qty: 1000, OrderType: "market",
			}}
		},
	}

	ticks := []strategy.Tick{
		tick("AAPL", 1, 100, 100),
		tick("AAPL", 2, 101, 102),
	}

	e := NewEngine(strat, 100) // only $100 cash
	r := e.Run(ticks)

	// Can't afford 1000 shares at ~101, so no trades
	if r.TotalTrades != 0 {
		t.Errorf("expected 0 trades due to insufficient cash, got %d", r.TotalTrades)
	}
}

func TestEngine_LastBarOrdersDropped(t *testing.T) {
	strat := &mockStrategy{
		name: "last_bar",
		onTick: func(tick strategy.Tick, p strategy.Portfolio) []strategy.Order {
			return []strategy.Order{{
				Symbol: "AAPL", Side: "buy", Qty: 1, OrderType: "market",
			}}
		},
	}

	ticks := []strategy.Tick{
		tick("AAPL", 1, 100, 100),
	}

	e := NewEngine(strat, 10000)
	r := e.Run(ticks)

	// Only one tick, no next bar to fill at
	if len(strat.fills) != 0 {
		t.Errorf("expected no fills for single tick, got %d", len(strat.fills))
	}
	if r.TotalTrades != 0 {
		t.Errorf("expected 0 trades, got %d", r.TotalTrades)
	}
}

func TestEngine_MultiSymbol(t *testing.T) {
	bought := map[string]bool{}
	strat := &mockStrategy{
		name: "multi",
		onTick: func(tick strategy.Tick, p strategy.Portfolio) []strategy.Order {
			if !bought[tick.Symbol] {
				bought[tick.Symbol] = true
				return []strategy.Order{{
					Symbol: tick.Symbol, Side: "buy", Qty: 1, OrderType: "market",
				}}
			}
			return nil
		},
	}

	ticks := []strategy.Tick{
		tick("AAPL", 1, 100, 100),
		tick("GOOG", 1, 200, 200),
		tick("AAPL", 2, 105, 105),
		tick("GOOG", 2, 210, 210),
	}

	e := NewEngine(strat, 10000)
	r := e.Run(ticks)

	// Both should have been bought (fills at next same-symbol bar's open)
	if len(strat.fills) != 2 {
		t.Errorf("expected 2 fills, got %d", len(strat.fills))
	}
	// No sells, so TotalTrades (wins+losses with realizedPL != 0) = 0
	if r.TotalTrades != 0 {
		t.Errorf("TotalTrades = %v, want 0 (buys have no realized P&L)", r.TotalTrades)
	}
}

// --- Helper function tests ---

func TestPrecomputeNextSameSymbol(t *testing.T) {
	ticks := []strategy.Tick{
		{Symbol: "A"}, // 0 -> 2
		{Symbol: "B"}, // 1 -> 3
		{Symbol: "A"}, // 2 -> -1
		{Symbol: "B"}, // 3 -> -1
	}
	next := precomputeNextSameSymbol(ticks)
	expected := []int{2, 3, -1, -1}
	for i, want := range expected {
		if next[i] != want {
			t.Errorf("next[%d] = %d, want %d", i, next[i], want)
		}
	}
}

func TestPrecomputeNextSameSymbol_Empty(t *testing.T) {
	next := precomputeNextSameSymbol(nil)
	if len(next) != 0 {
		t.Errorf("expected empty slice, got %v", next)
	}
}

func TestPrecomputeDayPrices(t *testing.T) {
	ticks := []strategy.Tick{
		{Symbol: "A", Timestamp: time.Date(2024, 1, 1, 9, 30, 0, 0, time.UTC), Open: 100, Close: 101},
		{Symbol: "B", Timestamp: time.Date(2024, 1, 1, 9, 31, 0, 0, time.UTC), Open: 200, Close: 201},
		{Symbol: "A", Timestamp: time.Date(2024, 1, 1, 15, 0, 0, 0, time.UTC), Open: 102, Close: 103},
		{Symbol: "A", Timestamp: time.Date(2024, 1, 2, 9, 30, 0, 0, time.UTC), Open: 104, Close: 105},
	}

	days := precomputeDayPrices(ticks)
	if len(days) != 2 {
		t.Fatalf("expected 2 days, got %d", len(days))
	}

	// Day 1
	if days[0].opens["A"] != 100 {
		t.Errorf("day1 A open = %v, want 100", days[0].opens["A"])
	}
	if days[0].closes["A"] != 103 {
		t.Errorf("day1 A close = %v, want 103 (last tick)", days[0].closes["A"])
	}
	if days[0].opens["B"] != 200 {
		t.Errorf("day1 B open = %v, want 200", days[0].opens["B"])
	}

	// Day 2
	if days[1].opens["A"] != 104 {
		t.Errorf("day2 A open = %v, want 104", days[1].opens["A"])
	}
}

func TestPrecomputeDayPrices_Empty(t *testing.T) {
	days := precomputeDayPrices(nil)
	if len(days) != 0 {
		t.Errorf("expected empty, got %v", days)
	}
}

func TestRecordEquity(t *testing.T) {
	var curve []float64
	peak := 0.0
	maxDD := 0.0

	recordEquity(100, &curve, &peak, &maxDD)
	if peak != 100 || maxDD != 0 {
		t.Errorf("after 100: peak=%v maxDD=%v", peak, maxDD)
	}

	recordEquity(120, &curve, &peak, &maxDD)
	if peak != 120 {
		t.Errorf("peak should be 120, got %v", peak)
	}

	recordEquity(90, &curve, &peak, &maxDD)
	// DD = (120-90)/120 * 100 = 25%
	if !approxEqual(maxDD, 25, 0.01) {
		t.Errorf("maxDD = %v, want 25", maxDD)
	}

	recordEquity(130, &curve, &peak, &maxDD)
	if peak != 130 {
		t.Errorf("peak should update to 130, got %v", peak)
	}
	// maxDD should still be 25% (the worst drawdown)
	if !approxEqual(maxDD, 25, 0.01) {
		t.Errorf("maxDD should remain 25, got %v", maxDD)
	}

	if len(curve) != 4 {
		t.Errorf("curve length = %d, want 4", len(curve))
	}
}

func TestSharpe(t *testing.T) {
	// Constant equity = 0 std = 0 sharpe
	if sharpe([]float64{100, 100, 100}) != 0 {
		t.Error("constant equity should give 0 sharpe")
	}

	// Single point
	if sharpe([]float64{100}) != 0 {
		t.Error("single point should give 0 sharpe")
	}

	// Empty
	if sharpe(nil) != 0 {
		t.Error("empty should give 0 sharpe")
	}

	// Monotonically increasing should give positive sharpe
	s := sharpe([]float64{100, 110, 121, 133.1})
	if s <= 0 {
		t.Errorf("expected positive sharpe for increasing equity, got %v", s)
	}

	// Monotonically decreasing should give negative sharpe
	s = sharpe([]float64{100, 90, 81, 72.9})
	if s >= 0 {
		t.Errorf("expected negative sharpe for decreasing equity, got %v", s)
	}
}

func TestDefaultDuration(t *testing.T) {
	const day = 24 * time.Hour
	tests := []struct {
		tf   string
		want time.Duration
	}{
		{"1m", 30 * day},
		{"5m", 30 * day},
		{"15m", 60 * day},
		{"30m", 60 * day},
		{"1h", 60 * day},
		{"1d", 365 * day},
		{"unknown", 30 * day},
	}
	for _, tt := range tests {
		t.Run(tt.tf, func(t *testing.T) {
			got := DefaultDuration(tt.tf)
			if got != tt.want {
				t.Errorf("DefaultDuration(%q) = %v, want %v", tt.tf, got, tt.want)
			}
		})
	}
}
