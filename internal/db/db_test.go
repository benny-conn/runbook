package db

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/benny-conn/brandon-bot/backtest"
	"github.com/benny-conn/brandon-bot/strategy"
)

// newTestStore creates an in-memory SQLite store for testing.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrate(t *testing.T) {
	s := newTestStore(t)

	// Verify tables exist by querying them
	tables := []string{"backtest_runs", "backtest_fills", "paper_orders", "paper_fills", "paper_snapshots"}
	for _, table := range tables {
		var count int
		err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count)
		if err != nil {
			t.Errorf("table %s not created: %v", table, err)
		}
	}
}

func TestMigrateIdempotent(t *testing.T) {
	s := newTestStore(t)
	// Running migrate again should not fail (CREATE TABLE IF NOT EXISTS)
	if err := s.migrate(); err != nil {
		t.Fatalf("second migrate failed: %v", err)
	}
}

func TestSaveBacktestRun(t *testing.T) {
	s := newTestStore(t)

	params := BacktestRunParams{
		Strategy:  "test_strat",
		Symbols:   []string{"AAPL", "GOOG"},
		Timeframe: "1m",
		From:      time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		To:        time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC),
	}
	results := &backtest.Results{
		InitialCapital: 10000,
		FinalEquity:    10500,
		TotalReturnPct: 5.0,
		MaxDrawdownPct: 2.0,
		SharpeRatio:    1.5,
		TotalTrades:    3,
		WinningTrades:  2,
		LosingTrades:   1,
		Trades: []backtest.Trade{
			{
				Fill:       strategy.Fill{Symbol: "AAPL", Side: "buy", Qty: 10, Price: 100, Timestamp: time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)},
				RealizedPL: 0,
			},
			{
				Fill:       strategy.Fill{Symbol: "AAPL", Side: "sell", Qty: 10, Price: 105, Timestamp: time.Date(2024, 1, 3, 10, 0, 0, 0, time.UTC)},
				RealizedPL: 50,
			},
		},
	}

	runID, err := s.SaveBacktestRun(params, results)
	if err != nil {
		t.Fatalf("SaveBacktestRun: %v", err)
	}
	if runID < 1 {
		t.Errorf("expected positive runID, got %d", runID)
	}

	// Verify the run was saved
	var strat string
	var symbols string
	var initialCapital float64
	err = s.db.QueryRow("SELECT strategy, symbols, initial_capital FROM backtest_runs WHERE id = ?", runID).
		Scan(&strat, &symbols, &initialCapital)
	if err != nil {
		t.Fatalf("query run: %v", err)
	}
	if strat != "test_strat" {
		t.Errorf("strategy = %q, want %q", strat, "test_strat")
	}
	if symbols != "AAPL,GOOG" {
		t.Errorf("symbols = %q, want %q", symbols, "AAPL,GOOG")
	}
	if initialCapital != 10000 {
		t.Errorf("initial_capital = %v, want 10000", initialCapital)
	}

	// Verify fills were saved
	var fillCount int
	err = s.db.QueryRow("SELECT COUNT(*) FROM backtest_fills WHERE run_id = ?", runID).Scan(&fillCount)
	if err != nil {
		t.Fatalf("query fills: %v", err)
	}
	if fillCount != 2 {
		t.Errorf("fill count = %d, want 2", fillCount)
	}
}

func TestSaveBacktestRun_NoTrades(t *testing.T) {
	s := newTestStore(t)

	params := BacktestRunParams{
		Strategy:  "empty",
		Symbols:   []string{"AAPL"},
		Timeframe: "1d",
		From:      time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		To:        time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC),
	}
	results := &backtest.Results{
		InitialCapital: 10000,
		FinalEquity:    10000,
	}

	runID, err := s.SaveBacktestRun(params, results)
	if err != nil {
		t.Fatalf("SaveBacktestRun: %v", err)
	}
	if runID < 1 {
		t.Errorf("expected positive runID, got %d", runID)
	}
}

func TestLogPaperOrder(t *testing.T) {
	s := newTestStore(t)

	order := strategy.Order{
		Symbol:    "AAPL",
		Side:      "buy",
		Qty:       10,
		OrderType: "market",
		Reason:    "test buy",
	}

	err := s.LogPaperOrder(order, "broker-123")
	if err != nil {
		t.Fatalf("LogPaperOrder: %v", err)
	}

	var symbol, side, reason string
	var qty float64
	err = s.db.QueryRow("SELECT symbol, side, qty, reason FROM paper_orders WHERE alpaca_order_id = ?", "broker-123").
		Scan(&symbol, &side, &qty, &reason)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if symbol != "AAPL" || side != "buy" || qty != 10 || reason != "test buy" {
		t.Errorf("got (%q, %q, %v, %q), want (AAPL, buy, 10, test buy)", symbol, side, qty, reason)
	}
}

func TestLogPaperFill(t *testing.T) {
	s := newTestStore(t)

	fill := strategy.Fill{
		Symbol:    "GOOG",
		Side:      "sell",
		Qty:       5,
		Price:     150.50,
		Timestamp: time.Date(2024, 6, 15, 14, 30, 0, 0, time.UTC),
	}

	err := s.LogPaperFill(fill)
	if err != nil {
		t.Fatalf("LogPaperFill: %v", err)
	}

	var symbol, side string
	var qty, price float64
	err = s.db.QueryRow("SELECT symbol, side, qty, price FROM paper_fills").
		Scan(&symbol, &side, &qty, &price)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if symbol != "GOOG" || side != "sell" || qty != 5 || price != 150.50 {
		t.Errorf("got (%q, %q, %v, %v)", symbol, side, qty, price)
	}
}

func TestLogPaperSnapshot(t *testing.T) {
	s := newTestStore(t)

	err := s.LogPaperSnapshot(9500, 10200, 200)
	if err != nil {
		t.Fatalf("LogPaperSnapshot: %v", err)
	}

	var cash, equity, totalPL float64
	err = s.db.QueryRow("SELECT cash, equity, total_pl FROM paper_snapshots").
		Scan(&cash, &equity, &totalPL)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if cash != 9500 || equity != 10200 || totalPL != 200 {
		t.Errorf("got (%v, %v, %v), want (9500, 10200, 200)", cash, equity, totalPL)
	}
}

func TestLogOrder_ImplementsEngineStore(t *testing.T) {
	s := newTestStore(t)
	// LogOrder/LogFill/LogSnapshot are wrappers — just verify they don't error
	err := s.LogOrder(strategy.Order{Symbol: "AAPL", Side: "buy", Qty: 1, OrderType: "market"}, "id-1")
	if err != nil {
		t.Fatalf("LogOrder: %v", err)
	}
	err = s.LogFill(strategy.Fill{Symbol: "AAPL", Side: "buy", Qty: 1, Price: 100, Timestamp: time.Now()})
	if err != nil {
		t.Fatalf("LogFill: %v", err)
	}
	err = s.LogSnapshot(9900, 10000, 100)
	if err != nil {
		t.Fatalf("LogSnapshot: %v", err)
	}
}
