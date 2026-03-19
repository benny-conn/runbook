# brandon-bot

A paper trading backtester and live simulator in Go. Test a day trading strategy against historical data or run it live against an Alpaca paper account without risking real money.

---

## Prerequisites

- Go 1.22+
- An [Alpaca Markets](https://alpaca.markets) account (free)
- Environment variables set in your shell (see below)

## Environment Variables

```bash
ALPACA_API_KEY=your_key_here
ALPACA_SECRET=your_secret_here
ALPACA_BASE_URL=https://paper-api.alpaca.markets/v2
DATABASE_PATH=./trading_bot.db   # optional, defaults to ./trading_bot.db
```

Get your API keys from the Alpaca dashboard under **Paper Trading** → **API Keys**.

---

## Running a Backtest

Fetches historical bars from Alpaca, replays them through the strategy, and prints a full performance report.

```bash
go run cmd/backtest/main.go \
  --strategy=ma_crossover \
  --symbols=AAPL,TSLA \
  --from=2024-01-01 \
  --to=2024-12-31 \
  --timeframe=1d \
  --capital=10000
```

**Flags:**

| Flag          | Default        | Description                             |
| ------------- | -------------- | --------------------------------------- |
| `--strategy`  | `ma_crossover` | Strategy to run                         |
| `--symbols`   | `AAPL`         | Comma-separated ticker list             |
| `--from`      | required       | Start date (YYYY-MM-DD)                 |
| `--to`        | required       | End date (YYYY-MM-DD)                   |
| `--timeframe` | `1d`           | Bar size: `1m`, `5m`, `15m`, `1h`, `1d` |
| `--capital`   | `10000`        | Starting capital in USD                 |

**Output:**

```
Fetching 1d bars for AAPL from 2024-01-01 to 2024-12-31...
Loaded 252 bars

=== Backtest Results ===
Initial capital:  $10000.00
Final equity:     $10054.73
Total return:     0.55%
Max drawdown:     0.62%
Sharpe ratio:     0.0405 (per-bar, not annualized)
Total trades:     3
Win rate:         0.0% (0 W / 3 L)

--- Trade Log ---
[2024-05-03 04:00] BUY  AAPL  qty=5.78  price=$186.65
...

Run saved to database (id=1)
```

Results and fills are saved to SQLite for later review.

---

## Running Paper Trading (Live)

Connects to Alpaca via WebSocket, streams real-time bars, and places orders against your paper account. This is a long-running process — it stays alive during market hours and shuts down cleanly on `Ctrl+C`.

```bash
go run cmd/paper/main.go \
  --strategy=ma_crossover \
  --symbols=AAPL,TSLA \
  --timeframe=1m \
  --capital=10000 \
  --feed=iex
```

**Flags:**

| Flag          | Default        | Description                                                                               |
| ------------- | -------------- | ----------------------------------------------------------------------------------------- |
| `--strategy`  | `ma_crossover` | Strategy to run                                                                           |
| `--symbols`   | `AAPL`         | Comma-separated ticker list                                                               |
| `--timeframe` | `1m`           | Bar size: `1m`, `5m`, `15m`, `1h`, `1d`                                                   |
| `--capital`   | `10000`        | Starting capital (used only on first run, subsequent runs seed from Alpaca account state) |
| `--feed`      | `iex`          | Market data feed: `iex` (free) or `sip` (paid)                                            |

**On startup**, the engine automatically:

1. Fetches your real account balance and open positions from Alpaca
2. Replays recent historical bars to warm up strategy indicators (EMAs, etc.)
3. Injects any existing positions into the strategy so it knows what it's holding
4. Then begins the live stream

This means **restarting the bot mid-session is safe** — it picks up from the correct state rather than thinking it has no positions.

---

## Project Structure

```
brandon-bot/
├── cmd/
│   ├── backtest/main.go     # CLI: run a backtest
│   └── paper/main.go        # CLI: run paper trading live
├── internal/
│   ├── strategy/
│   │   ├── strategy.go      # Core interfaces: Strategy, Portfolio, Tick, Order, Fill
│   │   └── ma_crossover.go  # Example: 9/21 EMA crossover strategy
│   ├── portfolio/
│   │   └── portfolio.go     # Tracks cash, positions, P&L
│   ├── market/
│   │   ├── alpaca.go        # Alpaca REST client (fetch historical bars)
│   │   └── historical.go    # Tick sorting utilities
│   ├── backtest/
│   │   └── engine.go        # Backtesting engine + performance metrics
│   ├── paper/
│   │   ├── engine.go        # Live paper trading engine (WebSocket loop)
│   │   └── recovery.go      # Startup state recovery from Alpaca
│   ├── execution/
│   │   ├── paper.go         # Submits orders to Alpaca REST API
│   │   └── simulated.go     # (placeholder for backtest fill logic)
│   ├── risk/
│   │   └── risk.go          # Position sizing helpers
│   └── db/
│       └── db.go            # SQLite logging (runs, fills, snapshots)
├── data/                    # Historical data cache (gitignored)
├── results/                 # Backtest output (gitignored)
├── go.mod
└── .env.example
```

---

## Adding a Custom Strategy

1. Create `internal/strategy/my_strategy.go`
2. Implement the `Strategy` interface:

```go
type MyStrategy struct {
    // your internal state (indicators, position tracking, etc.)
}

func (s *MyStrategy) Name() string { return "my_strategy" }

// Called on every bar. Return any orders to place — the engine handles execution.
// All state lives inside the strategy; the engine only sees what orders come back.
func (s *MyStrategy) OnTick(tick strategy.Tick, portfolio strategy.Portfolio) []strategy.Order {
    // your logic here
    return nil
}

// Called when an order is filled. Update your internal position tracking.
func (s *MyStrategy) OnFill(fill strategy.Fill) {}

// Optional: subscribe to individual trade prints instead of (or in addition to) bars.
// If this method exists, the paper engine automatically subscribes to the trade stream.
// OnTick is still called for bar events — implement it as a no-op if you don't need it.
func (s *MyStrategy) OnTrade(trade strategy.Trade, portfolio strategy.Portfolio) []strategy.Order {
    // react to every individual trade print (price, size, exchange, conditions)
    return nil
}

// Optional: support safe restarts mid-position.
// Called on startup if the bot already holds a position when it comes back up.
func (s *MyStrategy) SeedPosition(symbol string, qty, avgCost float64) {}
```

3. Register it in both `cmd/backtest/main.go` and `cmd/paper/main.go`:

```go
func resolveStrategy(name string) (strategy.Strategy, error) {
    switch name {
    case "ma_crossover":
        return strategy.NewMACrossover(), nil
    case "my_strategy":           // add this
        return strategy.NewMyStrategy(), nil
    default:
        return nil, fmt.Errorf("available strategies: ma_crossover, my_strategy")
    }
}
```

4. Run it:

```bash
go run cmd/backtest/main.go --strategy=my_strategy --symbols=AAPL --from=2024-01-01 --to=2024-12-31
```

---

## Built-in Strategy: MA Crossover

A simple 9/21 exponential moving average crossover, included as a working example to validate the engine. **Not intended for real use.**

- **Buy signal**: 9-period EMA crosses above 21-period EMA → buy 10% of available cash
- **Sell signal**: 9-period EMA crosses below 21-period EMA → sell full position
- **Stop loss**: price drops 2% below entry → sell immediately

---

## Live Trading (Future)

When ready to trade with real money, the architecture is identical — just swap the API keys and base URL:

```bash
ALPACA_BASE_URL=https://api.alpaca.markets/v2
```

A `cmd/live/main.go` will be added with additional safeguards (position limits, kill switch, etc.) before this is used.

---

## Database

Results are logged to SQLite at `DATABASE_PATH` (default: `./trading_bot.db`).

| Table             | Contents                                  |
| ----------------- | ----------------------------------------- |
| `backtest_runs`   | Summary metrics for each backtest run     |
| `backtest_fills`  | Individual fills from each run            |
| `paper_orders`    | Orders submitted during paper trading     |
| `paper_fills`     | Fill confirmations from Alpaca            |
| `paper_snapshots` | Portfolio snapshots taken after each fill |
