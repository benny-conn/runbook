package db

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"brandon-bot/internal/backtest"
	"brandon-bot/internal/strategy"
)

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database, running migrations if needed.
// Path is read from DATABASE_PATH env var; defaults to ./trading_bot.db.
func Open() (*Store, error) {
	path := os.Getenv("DATABASE_PATH")
	if path == "" {
		path = "./trading_bot.db"
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening database at %s: %w", path, err)
	}
	// SQLite performs best with a single writer connection.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS backtest_runs (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			strategy         TEXT    NOT NULL,
			symbols          TEXT    NOT NULL,
			timeframe        TEXT    NOT NULL,
			from_date        TEXT    NOT NULL,
			to_date          TEXT    NOT NULL,
			initial_capital  REAL    NOT NULL,
			final_equity     REAL    NOT NULL,
			total_return_pct REAL    NOT NULL,
			max_drawdown_pct REAL    NOT NULL,
			sharpe_ratio     REAL    NOT NULL,
			total_trades     INTEGER NOT NULL,
			winning_trades   INTEGER NOT NULL,
			losing_trades    INTEGER NOT NULL,
			run_at           TEXT    NOT NULL
		);

		CREATE TABLE IF NOT EXISTS backtest_fills (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id      INTEGER NOT NULL REFERENCES backtest_runs(id),
			symbol      TEXT    NOT NULL,
			side        TEXT    NOT NULL,
			qty         REAL    NOT NULL,
			price       REAL    NOT NULL,
			fill_time   TEXT    NOT NULL,
			realized_pl REAL    NOT NULL
		);

		-- Paper trading tables (populated in step 7)
		CREATE TABLE IF NOT EXISTS paper_orders (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			alpaca_order_id TEXT,
			symbol         TEXT    NOT NULL,
			side           TEXT    NOT NULL,
			qty            REAL    NOT NULL,
			order_type     TEXT    NOT NULL,
			reason         TEXT,
			submitted_at   TEXT    NOT NULL
		);

		CREATE TABLE IF NOT EXISTS paper_fills (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			order_id     INTEGER REFERENCES paper_orders(id),
			symbol       TEXT    NOT NULL,
			side         TEXT    NOT NULL,
			qty          REAL    NOT NULL,
			price        REAL    NOT NULL,
			fill_time    TEXT    NOT NULL
		);

		CREATE TABLE IF NOT EXISTS paper_snapshots (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			cash       REAL    NOT NULL,
			equity     REAL    NOT NULL,
			total_pl   REAL    NOT NULL,
			snapshot_at TEXT   NOT NULL
		);
	`)
	return err
}

// BacktestRunParams holds the CLI parameters that describe a backtest run.
type BacktestRunParams struct {
	Strategy  string
	Symbols   []string
	Timeframe string
	From      time.Time
	To        time.Time
}

// SaveBacktestRun persists a completed backtest run and all its fills.
// Returns the new run's ID.
func (s *Store) SaveBacktestRun(params BacktestRunParams, results *backtest.Results) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`
		INSERT INTO backtest_runs
			(strategy, symbols, timeframe, from_date, to_date,
			 initial_capital, final_equity, total_return_pct,
			 max_drawdown_pct, sharpe_ratio, total_trades,
			 winning_trades, losing_trades, run_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		params.Strategy,
		strings.Join(params.Symbols, ","),
		params.Timeframe,
		params.From.Format("2006-01-02"),
		params.To.Format("2006-01-02"),
		results.InitialCapital,
		results.FinalEquity,
		results.TotalReturnPct,
		results.MaxDrawdownPct,
		results.SharpeRatio,
		results.TotalTrades,
		results.WinningTrades,
		results.LosingTrades,
		time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("inserting run: %w", err)
	}

	runID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	for _, trade := range results.Trades {
		f := trade.Fill
		_, err = tx.Exec(`
			INSERT INTO backtest_fills
				(run_id, symbol, side, qty, price, fill_time, realized_pl)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`,
			runID,
			f.Symbol,
			f.Side,
			f.Qty,
			f.Price,
			f.Timestamp.UTC().Format(time.RFC3339),
			trade.RealizedPL,
		)
		if err != nil {
			return 0, fmt.Errorf("inserting fill: %w", err)
		}
	}

	return runID, tx.Commit()
}

// LogPaperOrder records a submitted order to paper_orders.
func (s *Store) LogPaperOrder(order strategy.Order, alpacaOrderID string) error {
	_, err := s.db.Exec(`
		INSERT INTO paper_orders
			(alpaca_order_id, symbol, side, qty, order_type, reason, submitted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`,
		alpacaOrderID,
		order.Symbol,
		order.Side,
		order.Qty,
		order.OrderType,
		order.Reason,
		time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

// LogPaperFill records a fill event to paper_fills.
func (s *Store) LogPaperFill(fill strategy.Fill) error {
	_, err := s.db.Exec(`
		INSERT INTO paper_fills
			(symbol, side, qty, price, fill_time)
		VALUES (?, ?, ?, ?, ?)
	`,
		fill.Symbol,
		fill.Side,
		fill.Qty,
		fill.Price,
		fill.Timestamp.UTC().Format(time.RFC3339),
	)
	return err
}

// LogPaperSnapshot records a portfolio snapshot taken after each fill.
func (s *Store) LogPaperSnapshot(cash, equity, totalPL float64) error {
	_, err := s.db.Exec(`
		INSERT INTO paper_snapshots
			(cash, equity, total_pl, snapshot_at)
		VALUES (?, ?, ?, ?)
	`,
		cash,
		equity,
		totalPL,
		time.Now().UTC().Format(time.RFC3339),
	)
	return err
}
