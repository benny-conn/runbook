package script

import (
	"math"
	"testing"
	"time"

	"github.com/benny-conn/brandon-bot/strategy"
)

const epsilon = 1e-6

func approxEqual(a, b, eps float64) bool {
	return math.Abs(a-b) < eps
}

// ---------------------------------------------------------------------------
// Mock portfolio
// ---------------------------------------------------------------------------

type mockPortfolio struct {
	cash      float64
	equity    float64
	totalPL   float64
	positions map[string]*strategy.Position
}

func newMockPortfolio() *mockPortfolio {
	return &mockPortfolio{
		cash:      100000,
		equity:    100000,
		totalPL:   0,
		positions: make(map[string]*strategy.Position),
	}
}

func (p *mockPortfolio) Cash() float64                          { return p.cash }
func (p *mockPortfolio) Equity() float64                        { return p.equity }
func (p *mockPortfolio) TotalPL() float64                       { return p.totalPL }
func (p *mockPortfolio) Position(symbol string) *strategy.Position { return p.positions[symbol] }
func (p *mockPortfolio) Positions() []strategy.Position {
	out := make([]strategy.Position, 0, len(p.positions))
	for _, pos := range p.positions {
		out = append(out, *pos)
	}
	return out
}

// helper to run a script and get the first order back
func runScript(t *testing.T, src string) []strategy.Order {
	t.Helper()
	s, err := New("test", src, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	tick := strategy.Tick{
		Symbol:    "TEST",
		Timestamp: time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC),
		Open:      100,
		High:      105,
		Low:       95,
		Close:     102,
		Volume:    1000,
	}
	s.SetPortfolio(newMockPortfolio())
	return s.OnBar("1m", tick)
}

// ---------------------------------------------------------------------------
// TA Library Tests
// ---------------------------------------------------------------------------

func TestTA_SMA(t *testing.T) {
	// SMA of [1,2,3,4,5] with period 3 = (3+4+5)/3 = 4.0
	src := `
function onBar(timeframe, tick, portfolio) {
	var val = ta.sma([1,2,3,4,5], 3);
	return [{ symbol: "TEST", side: "buy", qty: val, orderType: "market" }];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if !approxEqual(orders[0].Qty, 4.0, epsilon) {
		t.Errorf("SMA: got %f, want 4.0", orders[0].Qty)
	}
}

func TestTA_EMA(t *testing.T) {
	// EMA of [1,2,3,4,5] with period 3
	// Seed = (1+2+3)/3 = 2.0
	// k = 2/(3+1) = 0.5
	// i=3: 4*0.5 + 2.0*0.5 = 3.0
	// i=4: 5*0.5 + 3.0*0.5 = 4.0
	src := `
function onBar(timeframe, tick, portfolio) {
	var val = ta.ema([1,2,3,4,5], 3);
	return [{ symbol: "TEST", side: "buy", qty: val, orderType: "market" }];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if !approxEqual(orders[0].Qty, 4.0, epsilon) {
		t.Errorf("EMA: got %f, want 4.0", orders[0].Qty)
	}
}

func TestTA_RSI(t *testing.T) {
	// Construct a series where we know the RSI.
	// 14-period RSI on a monotonically increasing series: all gains, no losses => RSI = 100
	src := `
function onBar(timeframe, tick, portfolio) {
	var closes = [];
	for (var i = 0; i < 20; i++) closes.push(100 + i);
	var val = ta.rsi(closes, 14);
	return [{ symbol: "TEST", side: "buy", qty: val, orderType: "market" }];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if !approxEqual(orders[0].Qty, 100.0, epsilon) {
		t.Errorf("RSI (all gains): got %f, want 100.0", orders[0].Qty)
	}
}

func TestTA_RSI_AllLosses(t *testing.T) {
	// Monotonically decreasing => RSI = 0
	src := `
function onBar(timeframe, tick, portfolio) {
	var closes = [];
	for (var i = 0; i < 20; i++) closes.push(200 - i);
	var val = ta.rsi(closes, 14);
	return [{ symbol: "TEST", side: "buy", qty: val, orderType: "market" }];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if !approxEqual(orders[0].Qty, 0.0, epsilon) {
		t.Errorf("RSI (all losses): got %f, want 0.0", orders[0].Qty)
	}
}

func TestTA_MACD(t *testing.T) {
	// Test that MACD returns an object with macd, signal, histogram fields.
	// Use a long enough series and verify histogram = macd - signal.
	src := `
function onBar(timeframe, tick, portfolio) {
	var closes = [];
	for (var i = 0; i < 50; i++) closes.push(100 + Math.sin(i * 0.3) * 10);
	var m = ta.macd(closes, 12, 26, 9);
	var hist = m.macd - m.signal;
	// Encode macd in qty, signal in limitPrice, histogram check in stopPrice
	return [{
		symbol: "TEST", side: "buy", orderType: "market",
		qty: m.histogram,
		limitPrice: hist
	}];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	// histogram should equal macd - signal
	if !approxEqual(orders[0].Qty, orders[0].LimitPrice, epsilon) {
		t.Errorf("MACD histogram mismatch: histogram=%f, macd-signal=%f", orders[0].Qty, orders[0].LimitPrice)
	}
}

func TestTA_BollingerBands(t *testing.T) {
	// BB on [10,10,10,10,10] period=5 stddev=2 => middle=10, upper=10, lower=10 (zero stddev)
	src := `
function onBar(timeframe, tick, portfolio) {
	var bb = ta.bollingerBands([10,10,10,10,10], 5, 2);
	return [{
		symbol: "TEST", side: "buy", orderType: "market",
		qty: bb.middle,
		limitPrice: bb.upper,
		stopPrice: bb.lower
	}];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if !approxEqual(orders[0].Qty, 10.0, epsilon) {
		t.Errorf("BB middle: got %f, want 10.0", orders[0].Qty)
	}
	if !approxEqual(orders[0].LimitPrice, 10.0, epsilon) {
		t.Errorf("BB upper: got %f, want 10.0", orders[0].LimitPrice)
	}
	if !approxEqual(orders[0].StopPrice, 10.0, epsilon) {
		t.Errorf("BB lower: got %f, want 10.0", orders[0].StopPrice)
	}
}

func TestTA_BollingerBands_WithVariance(t *testing.T) {
	// BB on [1,2,3,4,5] period=5 stddev=2
	// mean = 3.0, variance = (4+1+0+1+4)/5 = 2.0, sd = sqrt(2)
	// upper = 3 + 2*sqrt(2), lower = 3 - 2*sqrt(2)
	src := `
function onBar(timeframe, tick, portfolio) {
	var bb = ta.bollingerBands([1,2,3,4,5], 5, 2);
	return [{
		symbol: "TEST", side: "buy", orderType: "market",
		qty: bb.middle,
		limitPrice: bb.upper,
		stopPrice: bb.lower
	}];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	wantMid := 3.0
	wantUpper := 3.0 + 2*math.Sqrt(2.0)
	wantLower := 3.0 - 2*math.Sqrt(2.0)
	if !approxEqual(orders[0].Qty, wantMid, epsilon) {
		t.Errorf("BB middle: got %f, want %f", orders[0].Qty, wantMid)
	}
	if !approxEqual(orders[0].LimitPrice, wantUpper, epsilon) {
		t.Errorf("BB upper: got %f, want %f", orders[0].LimitPrice, wantUpper)
	}
	if !approxEqual(orders[0].StopPrice, wantLower, epsilon) {
		t.Errorf("BB lower: got %f, want %f", orders[0].StopPrice, wantLower)
	}
}

func TestTA_ATR(t *testing.T) {
	// Simple ATR: 5 bars, period=3
	// highs:  [12,13,14,15,16]
	// lows:   [8, 9, 10,11,12]
	// closes: [10,11,12,13,14]
	// True ranges (from index 1): max(H-L, |H-prevC|, |L-prevC|)
	// i=1: max(4, |13-10|, |9-10|) = max(4,3,1) = 4
	// i=2: max(4, |14-11|, |10-11|) = max(4,3,1) = 4
	// i=3: max(4, |15-12|, |11-12|) = max(4,3,1) = 4
	// i=4: max(4, |16-13|, |12-13|) = max(4,3,1) = 4
	// Initial ATR (period=3) = (4+4+4)/3 = 4
	// Wilder smooth: (4*2 + 4)/3 = 4
	// ATR = 4.0
	src := `
function onBar(timeframe, tick, portfolio) {
	var val = ta.atr([12,13,14,15,16], [8,9,10,11,12], [10,11,12,13,14], 3);
	return [{ symbol: "TEST", side: "buy", qty: val, orderType: "market" }];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if !approxEqual(orders[0].Qty, 4.0, epsilon) {
		t.Errorf("ATR: got %f, want 4.0", orders[0].Qty)
	}
}

func TestTA_Crossover(t *testing.T) {
	// Crossover: fast was below slow, now above
	// fast = [1, 3], slow = [2, 2] => prev: 1<=2, curr: 3>2 => true
	src := `
function onBar(timeframe, tick, portfolio) {
	var cross = ta.crossover([1, 3], [2, 2]);
	return [{ symbol: "TEST", side: "buy", qty: cross ? 1 : 0, orderType: "market" }];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if !approxEqual(orders[0].Qty, 1.0, epsilon) {
		t.Errorf("crossover should be true, got qty=%f", orders[0].Qty)
	}
}

func TestTA_Crossover_False(t *testing.T) {
	// No crossover: fast was above and stays above
	src := `
function onBar(timeframe, tick, portfolio) {
	var cross = ta.crossover([3, 4], [2, 2]);
	return [{ symbol: "TEST", side: "buy", qty: cross ? 1 : 0, orderType: "market" }];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if !approxEqual(orders[0].Qty, 0.0, epsilon) {
		t.Errorf("crossover should be false, got qty=%f", orders[0].Qty)
	}
}

func TestTA_Crossunder(t *testing.T) {
	// Crossunder: fast was above slow, now below
	// fast = [3, 1], slow = [2, 2] => prev: 3>=2, curr: 1<2 => true
	src := `
function onBar(timeframe, tick, portfolio) {
	var cross = ta.crossunder([3, 1], [2, 2]);
	return [{ symbol: "TEST", side: "buy", qty: cross ? 1 : 0, orderType: "market" }];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if !approxEqual(orders[0].Qty, 1.0, epsilon) {
		t.Errorf("crossunder should be true, got qty=%f", orders[0].Qty)
	}
}

func TestTA_Percentile(t *testing.T) {
	// Percentile of [10,20,30,40,50] at 50th percentile = 30 (median)
	src := `
function onBar(timeframe, tick, portfolio) {
	var val = ta.percentile([10,20,30,40,50], 50);
	return [{ symbol: "TEST", side: "buy", qty: val, orderType: "market" }];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if !approxEqual(orders[0].Qty, 30.0, epsilon) {
		t.Errorf("percentile(50): got %f, want 30.0", orders[0].Qty)
	}
}

func TestTA_Percentile_25th(t *testing.T) {
	// 25th percentile of [10,20,30,40,50]
	// idx = 0.25 * 4 = 1.0 => sorted[1] = 20
	src := `
function onBar(timeframe, tick, portfolio) {
	var val = ta.percentile([10,20,30,40,50], 25);
	return [{ symbol: "TEST", side: "buy", qty: val, orderType: "market" }];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if !approxEqual(orders[0].Qty, 20.0, epsilon) {
		t.Errorf("percentile(25): got %f, want 20.0", orders[0].Qty)
	}
}

func TestTA_Returns(t *testing.T) {
	// returns([100, 110, 105]) = [0.1, -0.04545...]
	src := `
function onBar(timeframe, tick, portfolio) {
	var rets = ta.returns([100, 110, 105]);
	// Encode first two returns in qty and limitPrice
	return [{
		symbol: "TEST", side: "buy", orderType: "market",
		qty: rets[0],
		limitPrice: rets[1]
	}];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	wantR0 := 0.1
	wantR1 := (105.0 - 110.0) / 110.0
	if !approxEqual(orders[0].Qty, wantR0, epsilon) {
		t.Errorf("returns[0]: got %f, want %f", orders[0].Qty, wantR0)
	}
	if !approxEqual(orders[0].LimitPrice, wantR1, epsilon) {
		t.Errorf("returns[1]: got %f, want %f", orders[0].LimitPrice, wantR1)
	}
}

func TestTA_MaxDrawdown(t *testing.T) {
	// Equity curve: [100, 120, 90, 110]
	// Peak=100, dd=0; Peak=120, dd=0; dd=(120-90)/120=0.25; dd=(120-110)/120=0.0833
	// maxDD = 0.25
	src := `
function onBar(timeframe, tick, portfolio) {
	var val = ta.maxDrawdown([100, 120, 90, 110]);
	return [{ symbol: "TEST", side: "buy", qty: val, orderType: "market" }];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if !approxEqual(orders[0].Qty, 0.25, epsilon) {
		t.Errorf("maxDrawdown: got %f, want 0.25", orders[0].Qty)
	}
}

func TestTA_Sharpe(t *testing.T) {
	// Sharpe ratio with known returns and rf=0
	// returns = [0.01, 0.02, 0.01, 0.02, 0.01]
	// mean = 0.014, sd = sqrt(((0.004^2)*3 + (0.006^2)*2)/5) = sqrt((0.000048+0.000072)/5)
	// = sqrt(0.000024) = 0.004899
	// sharpe = (0.014 - 0) / 0.004899 * sqrt(252)
	src := `
function onBar(timeframe, tick, portfolio) {
	var val = ta.sharpe([0.01, 0.02, 0.01, 0.02, 0.01], 0);
	return [{ symbol: "TEST", side: "buy", qty: val, orderType: "market" }];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	// Compute expected
	rets := []float64{0.01, 0.02, 0.01, 0.02, 0.01}
	mean := 0.0
	for _, r := range rets {
		mean += r
	}
	mean /= 5.0
	variance := 0.0
	for _, r := range rets {
		d := r - mean
		variance += d * d
	}
	sd := math.Sqrt(variance / 5.0)
	want := (mean / sd) * math.Sqrt(252)
	if !approxEqual(orders[0].Qty, want, 0.01) {
		t.Errorf("Sharpe: got %f, want %f", orders[0].Qty, want)
	}
}

// ---------------------------------------------------------------------------
// Order Parsing Tests
// ---------------------------------------------------------------------------

func TestParseOrders_AllFields(t *testing.T) {
	src := `
function onBar(timeframe, tick, portfolio) {
	return [{
		symbol: "AAPL",
		side: "buy",
		orderType: "stop_limit",
		qty: 10,
		limitPrice: 150.50,
		stopPrice: 149.00,
		slDistance: 145.00,
		tpDistance: 160.00,
		reason: "breakout signal"
	}];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	o := orders[0]
	if o.Symbol != "AAPL" {
		t.Errorf("symbol: got %q, want %q", o.Symbol, "AAPL")
	}
	if o.Side != "buy" {
		t.Errorf("side: got %q, want %q", o.Side, "buy")
	}
	if o.OrderType != "stop_limit" {
		t.Errorf("orderType: got %q, want %q", o.OrderType, "stop_limit")
	}
	if !approxEqual(o.Qty, 10.0, epsilon) {
		t.Errorf("qty: got %f, want 10.0", o.Qty)
	}
	if !approxEqual(o.LimitPrice, 150.50, epsilon) {
		t.Errorf("limitPrice: got %f, want 150.50", o.LimitPrice)
	}
	if !approxEqual(o.StopPrice, 149.00, epsilon) {
		t.Errorf("stopPrice: got %f, want 149.00", o.StopPrice)
	}
	if !approxEqual(o.SLDistance, 145.00, epsilon) {
		t.Errorf("slDistance: got %f, want 145.00", o.SLDistance)
	}
	if !approxEqual(o.TPDistance, 160.00, epsilon) {
		t.Errorf("tpDistance: got %f, want 160.00", o.TPDistance)
	}
	if o.Reason != "breakout signal" {
		t.Errorf("reason: got %q, want %q", o.Reason, "breakout signal")
	}
}

func TestParseOrders_MultipleOrders(t *testing.T) {
	src := `
function onBar(timeframe, tick, portfolio) {
	return [
		{ symbol: "AAPL", side: "buy", qty: 5, orderType: "limit", limitPrice: 150, reason: "entry" },
		{ symbol: "GOOG", side: "sell", qty: 3, orderType: "market", slDistance: 100, tpDistance: 200 }
	];
}
`
	orders := runScript(t, src)
	if len(orders) != 2 {
		t.Fatalf("expected 2 orders, got %d", len(orders))
	}
	if orders[0].Symbol != "AAPL" || orders[0].Reason != "entry" {
		t.Errorf("order[0] unexpected: %+v", orders[0])
	}
	if orders[1].Symbol != "GOOG" || !approxEqual(orders[1].SLDistance, 100.0, epsilon) {
		t.Errorf("order[1] unexpected: %+v", orders[1])
	}
	if !approxEqual(orders[1].TPDistance, 200.0, epsilon) {
		t.Errorf("order[1] tpDistance: got %f, want 200.0", orders[1].TPDistance)
	}
}

func TestParseOrders_EmptyReturn(t *testing.T) {
	src := `
function onBar(timeframe, tick, portfolio) {
	return [];
}
`
	orders := runScript(t, src)
	if len(orders) != 0 {
		t.Errorf("expected 0 orders, got %d", len(orders))
	}
}

func TestParseOrders_NullReturn(t *testing.T) {
	src := `
function onBar(timeframe, tick, portfolio) {
	return null;
}
`
	orders := runScript(t, src)
	if orders != nil && len(orders) != 0 {
		t.Errorf("expected nil/empty orders for null return, got %d", len(orders))
	}
}

func TestParseOrders_IntegerPrices(t *testing.T) {
	// Verify integer prices are correctly extracted (int64 -> float64 path)
	src := `
function onBar(timeframe, tick, portfolio) {
	return [{
		symbol: "TEST",
		side: "buy",
		orderType: "limit",
		qty: 1,
		limitPrice: 100,
		stopPrice: 95,
		slDistance: 90,
		tpDistance: 110
	}];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	o := orders[0]
	if !approxEqual(o.LimitPrice, 100.0, epsilon) {
		t.Errorf("limitPrice (int): got %f, want 100.0", o.LimitPrice)
	}
	if !approxEqual(o.StopPrice, 95.0, epsilon) {
		t.Errorf("stopPrice (int): got %f, want 95.0", o.StopPrice)
	}
	if !approxEqual(o.SLDistance, 90.0, epsilon) {
		t.Errorf("slDistance (int): got %f, want 90.0", o.SLDistance)
	}
	if !approxEqual(o.TPDistance, 110.0, epsilon) {
		t.Errorf("tpDistance (int): got %f, want 110.0", o.TPDistance)
	}
}

// ---------------------------------------------------------------------------
// ML Library Tests
// ---------------------------------------------------------------------------

func TestML_LogisticRegression(t *testing.T) {
	// Train on linearly separable data: x > 5 => label 1, else 0
	// Then predict for x=1 (expect < 0.5) and x=9 (expect > 0.5)
	src := `
function onBar(timeframe, tick, portfolio) {
	var features = [[1],[2],[3],[4],[5],[6],[7],[8],[9],[10]];
	var labels   = [ 0,  0,  0,  0,  0,  1,  1,  1,  1,  1 ];
	var model = ml.logisticRegression(features, labels, { maxIter: 1000, lr: 0.5 });
	var predLow = model.predict([1]);
	var predHigh = model.predict([9]);
	return [{
		symbol: "TEST", side: "buy", orderType: "market",
		qty: predLow,
		limitPrice: predHigh
	}];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if orders[0].Qty >= 0.5 {
		t.Errorf("logistic regression: predict([1]) = %f, want < 0.5", orders[0].Qty)
	}
	if orders[0].LimitPrice <= 0.5 {
		t.Errorf("logistic regression: predict([9]) = %f, want > 0.5", orders[0].LimitPrice)
	}
}

func TestML_DecisionTree(t *testing.T) {
	// Train on simple threshold: feature > 5 => 1, else 0
	src := `
function onBar(timeframe, tick, portfolio) {
	var features = [[1],[2],[3],[4],[5],[6],[7],[8],[9],[10]];
	var labels   = [ 0,  0,  0,  0,  0,  1,  1,  1,  1,  1 ];
	var model = ml.decisionTree(features, labels, { maxDepth: 5, minSamples: 1 });
	var predLow = model.predict([2]);
	var predHigh = model.predict([8]);
	return [{
		symbol: "TEST", side: "buy", orderType: "market",
		qty: predLow,
		limitPrice: predHigh
	}];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if !approxEqual(orders[0].Qty, 0.0, epsilon) {
		t.Errorf("decision tree: predict([2]) = %f, want 0.0", orders[0].Qty)
	}
	if !approxEqual(orders[0].LimitPrice, 1.0, epsilon) {
		t.Errorf("decision tree: predict([8]) = %f, want 1.0", orders[0].LimitPrice)
	}
}

func TestML_RandomForest(t *testing.T) {
	// Train on linearly separable 2D data
	src := `
function onBar(timeframe, tick, portfolio) {
	var features = [];
	var labels = [];
	for (var i = 0; i < 50; i++) {
		features.push([i, i * 2]);
		labels.push(i < 25 ? 0 : 1);
	}
	var model = ml.randomForest(features, labels, { nTrees: 20, maxDepth: 5, minSamples: 2 });
	var predLow = model.predict([5, 10]);
	var predHigh = model.predict([40, 80]);
	return [{
		symbol: "TEST", side: "buy", orderType: "market",
		qty: predLow,
		limitPrice: predHigh
	}];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if !approxEqual(orders[0].Qty, 0.0, epsilon) {
		t.Errorf("random forest: predict low = %f, want 0.0", orders[0].Qty)
	}
	if !approxEqual(orders[0].LimitPrice, 1.0, epsilon) {
		t.Errorf("random forest: predict high = %f, want 1.0", orders[0].LimitPrice)
	}
}

func TestML_KMeans(t *testing.T) {
	// Three well-separated clusters
	src := `
function onBar(timeframe, tick, portfolio) {
	var features = [
		[0, 0], [1, 0], [0, 1], [1, 1],
		[10, 10], [11, 10], [10, 11], [11, 11],
		[20, 0], [21, 0], [20, 1], [21, 1]
	];
	var model = ml.kmeans(features, { k: 3, maxIter: 100 });
	// Points near [0,0] and [10,10] should be in different clusters
	var c1 = model.predict([0, 0]);
	var c2 = model.predict([10, 10]);
	var c3 = model.predict([20, 0]);
	// All three clusters should be different
	var allDiff = (c1 !== c2 && c2 !== c3 && c1 !== c3) ? 1 : 0;
	return [{
		symbol: "TEST", side: "buy", orderType: "market",
		qty: allDiff
	}];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if !approxEqual(orders[0].Qty, 1.0, epsilon) {
		t.Errorf("kmeans: expected 3 distinct clusters, got same cluster assignments")
	}
}

func TestML_Normalize(t *testing.T) {
	// normalize([[0,10],[5,20],[10,30]]) =>
	// min=[0,10], max=[10,30], range=[10,20]
	// [[0,0],[0.5,0.5],[1,1]]
	src := `
function onBar(timeframe, tick, portfolio) {
	var result = ml.normalize([[0,10],[5,20],[10,30]]);
	return [{
		symbol: "TEST", side: "buy", orderType: "market",
		qty: result[0][0],
		limitPrice: result[1][0],
		stopPrice: result[2][0],
		slDistance: result[1][1]
	}];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if !approxEqual(orders[0].Qty, 0.0, epsilon) {
		t.Errorf("normalize [0][0]: got %f, want 0.0", orders[0].Qty)
	}
	if !approxEqual(orders[0].LimitPrice, 0.5, epsilon) {
		t.Errorf("normalize [1][0]: got %f, want 0.5", orders[0].LimitPrice)
	}
	if !approxEqual(orders[0].StopPrice, 1.0, epsilon) {
		t.Errorf("normalize [2][0]: got %f, want 1.0", orders[0].StopPrice)
	}
	if !approxEqual(orders[0].SLDistance, 0.5, epsilon) {
		t.Errorf("normalize [1][1]: got %f, want 0.5", orders[0].SLDistance)
	}
}

func TestML_Standardize(t *testing.T) {
	// standardize([[1],[3],[5]])
	// mean=3, std=sqrt((4+0+4)/3)=sqrt(8/3)
	// [0] = (1-3)/std, [1] = 0, [2] = (5-3)/std
	src := `
function onBar(timeframe, tick, portfolio) {
	var result = ml.standardize([[1],[3],[5]]);
	return [{
		symbol: "TEST", side: "buy", orderType: "market",
		qty: result[1][0],
		limitPrice: result[0][0],
		stopPrice: result[2][0]
	}];
}
`
	orders := runScript(t, src)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	// Middle value (3) standardized to 0
	if !approxEqual(orders[0].Qty, 0.0, epsilon) {
		t.Errorf("standardize [1][0] (mean): got %f, want 0.0", orders[0].Qty)
	}
	// First and last should be symmetric
	if !approxEqual(orders[0].LimitPrice, -orders[0].StopPrice, epsilon) {
		t.Errorf("standardize: expected symmetry, got [0]=%f, [2]=%f", orders[0].LimitPrice, orders[0].StopPrice)
	}
	// Verify actual value: (1-3)/sqrt(8/3) = -2/1.6329... = -1.2247...
	std := math.Sqrt(8.0 / 3.0)
	wantFirst := -2.0 / std
	if !approxEqual(orders[0].LimitPrice, wantFirst, epsilon) {
		t.Errorf("standardize [0][0]: got %f, want %f", orders[0].LimitPrice, wantFirst)
	}
}

// ---------------------------------------------------------------------------
// Portfolio mock integration test
// ---------------------------------------------------------------------------

func TestPortfolioAccess(t *testing.T) {
	src := `
function onBar(timeframe, tick) {
	var c = portfolio.cash();
	var e = portfolio.equity();
	var pl = portfolio.totalPL();
	return [{
		symbol: "TEST", side: "buy", orderType: "market",
		qty: c,
		limitPrice: e,
		stopPrice: pl
	}];
}
`
	s, err := New("test", src, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	p := newMockPortfolio()
	p.cash = 50000
	p.equity = 75000
	p.totalPL = 2500

	tick := strategy.Tick{
		Symbol:    "TEST",
		Timestamp: time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC),
		Open:      100, High: 105, Low: 95, Close: 102, Volume: 1000,
	}
	s.SetPortfolio(p)
	orders := s.OnBar("1m", tick)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if !approxEqual(orders[0].Qty, 50000, epsilon) {
		t.Errorf("cash: got %f, want 50000", orders[0].Qty)
	}
	if !approxEqual(orders[0].LimitPrice, 75000, epsilon) {
		t.Errorf("equity: got %f, want 75000", orders[0].LimitPrice)
	}
	if !approxEqual(orders[0].StopPrice, 2500, epsilon) {
		t.Errorf("totalPL: got %f, want 2500", orders[0].StopPrice)
	}
}

func TestPortfolioPosition(t *testing.T) {
	src := `
function onBar(timeframe, tick) {
	var pos = portfolio.position("AAPL");
	if (!pos) return [];
	return [{
		symbol: pos.symbol, side: "buy", orderType: "market",
		qty: pos.qty,
		limitPrice: pos.avgCost
	}];
}
`
	s, err := New("test", src, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	p := newMockPortfolio()
	p.positions["AAPL"] = &strategy.Position{
		Symbol:  "AAPL",
		Qty:     100,
		AvgCost: 150.25,
	}

	tick := strategy.Tick{
		Symbol:    "AAPL",
		Timestamp: time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC),
		Open:      100, High: 105, Low: 95, Close: 102, Volume: 1000,
	}
	s.SetPortfolio(p)
	orders := s.OnBar("1m", tick)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if orders[0].Symbol != "AAPL" {
		t.Errorf("symbol: got %q, want AAPL", orders[0].Symbol)
	}
	if !approxEqual(orders[0].Qty, 100, epsilon) {
		t.Errorf("qty: got %f, want 100", orders[0].Qty)
	}
	if !approxEqual(orders[0].LimitPrice, 150.25, epsilon) {
		t.Errorf("avgCost: got %f, want 150.25", orders[0].LimitPrice)
	}
}
