package portfolio

import (
	"math"
	"sync"
	"testing"

	"github.com/benny-conn/brandon-bot/strategy"
)

func approxEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

func TestNewSimulatedPortfolio(t *testing.T) {
	p := NewSimulatedPortfolio(10000)
	if p.Cash() != 10000 {
		t.Fatalf("Cash() = %v, want 10000", p.Cash())
	}
	if p.Equity() != 10000 {
		t.Fatalf("Equity() = %v, want 10000", p.Equity())
	}
	if len(p.Positions()) != 0 {
		t.Fatalf("expected no positions")
	}
	if p.Position("AAPL") != nil {
		t.Fatalf("expected nil position for unknown symbol")
	}
}

func TestApplyFill_Buy(t *testing.T) {
	p := NewSimulatedPortfolio(10000)
	p.ApplyFill(strategy.Fill{Symbol: "AAPL", Side: "buy", Qty: 10, Price: 100})

	if !approxEqual(p.Cash(), 9000, 0.01) {
		t.Errorf("Cash() = %v, want 9000", p.Cash())
	}
	pos := p.Position("AAPL")
	if pos == nil {
		t.Fatal("expected AAPL position")
	}
	if pos.Qty != 10 {
		t.Errorf("Qty = %v, want 10", pos.Qty)
	}
	if pos.AvgCost != 100 {
		t.Errorf("AvgCost = %v, want 100", pos.AvgCost)
	}
}

func TestApplyFill_BuyAddToPosition(t *testing.T) {
	p := NewSimulatedPortfolio(20000)
	p.ApplyFill(strategy.Fill{Symbol: "AAPL", Side: "buy", Qty: 10, Price: 100})
	p.ApplyFill(strategy.Fill{Symbol: "AAPL", Side: "buy", Qty: 10, Price: 120})

	pos := p.Position("AAPL")
	if pos.Qty != 20 {
		t.Errorf("Qty = %v, want 20", pos.Qty)
	}
	// Weighted avg: (10*100 + 10*120) / 20 = 110
	if !approxEqual(pos.AvgCost, 110, 0.01) {
		t.Errorf("AvgCost = %v, want 110", pos.AvgCost)
	}
}

func TestApplyFill_SellClosePosition(t *testing.T) {
	p := NewSimulatedPortfolio(10000)
	p.ApplyFill(strategy.Fill{Symbol: "AAPL", Side: "buy", Qty: 10, Price: 100})
	p.ApplyFill(strategy.Fill{Symbol: "AAPL", Side: "sell", Qty: 10, Price: 110})

	if !approxEqual(p.Cash(), 10100, 0.01) {
		t.Errorf("Cash() = %v, want 10100", p.Cash())
	}
	if p.Position("AAPL") != nil {
		t.Error("expected position to be closed")
	}
}

func TestApplyFill_SellPartial(t *testing.T) {
	p := NewSimulatedPortfolio(10000)
	p.ApplyFill(strategy.Fill{Symbol: "AAPL", Side: "buy", Qty: 10, Price: 100})
	p.ApplyFill(strategy.Fill{Symbol: "AAPL", Side: "sell", Qty: 5, Price: 110})

	pos := p.Position("AAPL")
	if pos == nil || pos.Qty != 5 {
		t.Errorf("expected 5 shares remaining, got %v", pos)
	}
}

func TestApplyFill_ShortPosition(t *testing.T) {
	p := NewSimulatedPortfolio(10000)
	p.ApplyFill(strategy.Fill{Symbol: "AAPL", Side: "sell", Qty: 10, Price: 100})

	if !approxEqual(p.Cash(), 11000, 0.01) {
		t.Errorf("Cash() = %v, want 11000", p.Cash())
	}
	pos := p.Position("AAPL")
	if pos == nil || pos.Qty != -10 {
		t.Errorf("expected short position of -10, got %v", pos)
	}
}

func TestApplyFill_CoverShort(t *testing.T) {
	p := NewSimulatedPortfolio(10000)
	p.ApplyFill(strategy.Fill{Symbol: "AAPL", Side: "sell", Qty: 10, Price: 100})
	p.ApplyFill(strategy.Fill{Symbol: "AAPL", Side: "buy", Qty: 10, Price: 90})

	// Sold at 100 (+1000), bought at 90 (-900), net cash = 10000 + 1000 - 900 = 10100
	if !approxEqual(p.Cash(), 10100, 0.01) {
		t.Errorf("Cash() = %v, want 10100", p.Cash())
	}
	if p.Position("AAPL") != nil {
		t.Error("expected position to be closed after covering short")
	}
}

func TestApplyFill_FlipLongToShort(t *testing.T) {
	p := NewSimulatedPortfolio(10000)
	p.ApplyFill(strategy.Fill{Symbol: "AAPL", Side: "buy", Qty: 5, Price: 100})
	p.ApplyFill(strategy.Fill{Symbol: "AAPL", Side: "sell", Qty: 8, Price: 110})

	pos := p.Position("AAPL")
	if pos == nil || pos.Qty != -3 {
		t.Errorf("expected short position of -3, got %v", pos)
	}
	if pos.AvgCost != 110 {
		t.Errorf("AvgCost should reset to 110 on flip, got %v", pos.AvgCost)
	}
}

func TestApplyFill_FlipShortToLong(t *testing.T) {
	p := NewSimulatedPortfolio(10000)
	p.ApplyFill(strategy.Fill{Symbol: "AAPL", Side: "sell", Qty: 5, Price: 100})
	p.ApplyFill(strategy.Fill{Symbol: "AAPL", Side: "buy", Qty: 8, Price: 90})

	pos := p.Position("AAPL")
	if pos == nil || pos.Qty != 3 {
		t.Errorf("expected long position of 3, got %v", pos)
	}
	if pos.AvgCost != 90 {
		t.Errorf("AvgCost should reset to 90 on flip, got %v", pos.AvgCost)
	}
}

func TestUpdateMarketPrice_Long(t *testing.T) {
	p := NewSimulatedPortfolio(10000)
	p.ApplyFill(strategy.Fill{Symbol: "AAPL", Side: "buy", Qty: 10, Price: 100})
	p.UpdateMarketPrice("AAPL", 110)

	pos := p.Position("AAPL")
	if !approxEqual(pos.MarketValue, 1100, 0.01) {
		t.Errorf("MarketValue = %v, want 1100", pos.MarketValue)
	}
	if !approxEqual(pos.UnrealizedPL, 100, 0.01) {
		t.Errorf("UnrealizedPL = %v, want 100", pos.UnrealizedPL)
	}
	// Equity = cash + market value = 9000 + 1100 = 10100
	if !approxEqual(p.Equity(), 10100, 0.01) {
		t.Errorf("Equity() = %v, want 10100", p.Equity())
	}
}

func TestUpdateMarketPrice_Short(t *testing.T) {
	p := NewSimulatedPortfolio(10000)
	p.ApplyFill(strategy.Fill{Symbol: "AAPL", Side: "sell", Qty: 10, Price: 100})
	p.UpdateMarketPrice("AAPL", 90)

	pos := p.Position("AAPL")
	// Short: market value is negative
	if !approxEqual(pos.MarketValue, -900, 0.01) {
		t.Errorf("MarketValue = %v, want -900", pos.MarketValue)
	}
	// Short P&L: (100-90)*10 = 100
	if !approxEqual(pos.UnrealizedPL, 100, 0.01) {
		t.Errorf("UnrealizedPL = %v, want 100", pos.UnrealizedPL)
	}
}

func TestUpdateMarketPrice_NoPosition(t *testing.T) {
	p := NewSimulatedPortfolio(10000)
	// Should not panic
	p.UpdateMarketPrice("AAPL", 100)
}

func TestTotalPL(t *testing.T) {
	p := NewSimulatedPortfolio(10000)
	p.ApplyFill(strategy.Fill{Symbol: "AAPL", Side: "buy", Qty: 10, Price: 100})
	p.ApplyFill(strategy.Fill{Symbol: "GOOG", Side: "buy", Qty: 5, Price: 200})
	p.UpdateMarketPrice("AAPL", 110)
	p.UpdateMarketPrice("GOOG", 190)

	// AAPL unrealized: (110-100)*10 = 100
	// GOOG unrealized: (190-200)*5 = -50
	if !approxEqual(p.TotalPL(), 50, 0.01) {
		t.Errorf("TotalPL() = %v, want 50", p.TotalPL())
	}
}

func TestPositionReturnsCopy(t *testing.T) {
	p := NewSimulatedPortfolio(10000)
	p.ApplyFill(strategy.Fill{Symbol: "AAPL", Side: "buy", Qty: 10, Price: 100})

	pos := p.Position("AAPL")
	pos.Qty = 999 // mutate the copy

	original := p.Position("AAPL")
	if original.Qty != 10 {
		t.Error("Position() should return a copy; internal state was mutated")
	}
}

// --- Futures (multiplier) tests ---

func TestFutures_PLWithMultiplier(t *testing.T) {
	// MNQ: point_value = 2.0
	p := NewSimulatedPortfolio(50000)
	p.SetMultipliers(map[string]float64{"MNQ": 2.0})

	// Buy 1 contract at 24470
	p.ApplyFill(strategy.Fill{Symbol: "MNQ", Side: "buy", Qty: 1, Price: 24470})

	// Cash should NOT decrease by notional (futures don't exchange notional)
	if !approxEqual(p.Cash(), 50000, 0.01) {
		t.Errorf("Cash() = %v, want 50000 (futures: no notional deduction)", p.Cash())
	}

	// Update market price to 24474 (4 point gain)
	p.UpdateMarketPrice("MNQ", 24474)
	pos := p.Position("MNQ")

	// Unrealized P&L = 4 points × 1 contract × $2/point = $8
	if !approxEqual(pos.UnrealizedPL, 8, 0.01) {
		t.Errorf("UnrealizedPL = %v, want 8", pos.UnrealizedPL)
	}

	// For futures, MarketValue IS the unrealized P&L
	if !approxEqual(pos.MarketValue, 8, 0.01) {
		t.Errorf("MarketValue = %v, want 8 (should equal unrealized P&L for futures)", pos.MarketValue)
	}

	// Equity = cash + unrealized P&L = 50000 + 8 = 50008
	if !approxEqual(p.Equity(), 50008, 0.01) {
		t.Errorf("Equity() = %v, want 50008", p.Equity())
	}
}

func TestFutures_RealizedPL(t *testing.T) {
	// ES: point_value = 50.0
	p := NewSimulatedPortfolio(50000)
	p.SetMultipliers(map[string]float64{"ES": 50.0})

	p.ApplyFill(strategy.Fill{Symbol: "ES", Side: "buy", Qty: 1, Price: 5800})
	p.ApplyFill(strategy.Fill{Symbol: "ES", Side: "sell", Qty: 1, Price: 5801})

	// Realized P&L = 1 point × 1 contract × $50/point = $50
	// Cash should be initial + realized P&L
	if !approxEqual(p.Cash(), 50050, 0.01) {
		t.Errorf("Cash() = %v, want 50050", p.Cash())
	}
	if p.Position("ES") != nil {
		t.Error("expected position to be closed")
	}
}

func TestFutures_ShortPL(t *testing.T) {
	// MNQ: point_value = 2.0
	p := NewSimulatedPortfolio(50000)
	p.SetMultipliers(map[string]float64{"MNQ": 2.0})

	// Short 2 contracts at 24470
	p.ApplyFill(strategy.Fill{Symbol: "MNQ", Side: "sell", Qty: 2, Price: 24470})

	// Cash unchanged (futures)
	if !approxEqual(p.Cash(), 50000, 0.01) {
		t.Errorf("Cash() = %v, want 50000", p.Cash())
	}

	// Cover at 24460 (10 point gain per contract)
	p.ApplyFill(strategy.Fill{Symbol: "MNQ", Side: "buy", Qty: 2, Price: 24460})

	// Realized P&L = 10 points × 2 contracts × $2/point = $40
	if !approxEqual(p.Cash(), 50040, 0.01) {
		t.Errorf("Cash() = %v, want 50040", p.Cash())
	}
}

func TestFutures_ComputeFillPL(t *testing.T) {
	p := NewSimulatedPortfolio(50000)
	p.SetMultipliers(map[string]float64{"MNQ": 2.0})

	p.ApplyFill(strategy.Fill{Symbol: "MNQ", Side: "buy", Qty: 1, Price: 24470})

	// Compute P&L before applying the closing fill
	closeFill := strategy.Fill{Symbol: "MNQ", Side: "sell", Qty: 1, Price: 24474}
	pl := p.ComputeFillPL(closeFill)

	// Should be 4 points × 1 × $2 = $8
	if !approxEqual(pl, 8, 0.01) {
		t.Errorf("ComputeFillPL = %v, want 8", pl)
	}
}

func TestFutures_EquityBehaviorUnchanged(t *testing.T) {
	// Without multipliers, everything should work as before (equity behavior)
	p := NewSimulatedPortfolio(10000)
	p.ApplyFill(strategy.Fill{Symbol: "AAPL", Side: "buy", Qty: 10, Price: 100})

	if !approxEqual(p.Cash(), 9000, 0.01) {
		t.Errorf("Cash() = %v, want 9000 (equity: notional deducted)", p.Cash())
	}
	p.UpdateMarketPrice("AAPL", 110)
	if !approxEqual(p.Equity(), 10100, 0.01) {
		t.Errorf("Equity() = %v, want 10100", p.Equity())
	}
}

func TestConcurrentAccess(t *testing.T) {
	p := NewSimulatedPortfolio(100000)
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.ApplyFill(strategy.Fill{Symbol: "AAPL", Side: "buy", Qty: 1, Price: 100})
			p.Cash()
			p.Equity()
			p.Position("AAPL")
			p.Positions()
			p.TotalPL()
			p.UpdateMarketPrice("AAPL", 105)
		}()
	}
	wg.Wait()

	pos := p.Position("AAPL")
	if pos == nil || pos.Qty != 100 {
		t.Errorf("expected 100 shares after concurrent buys, got %v", pos)
	}
}
