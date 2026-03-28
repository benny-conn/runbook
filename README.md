# runbook

A trading engine in Go that runs JavaScript strategies against live brokers or historical data. Design a strategy in JS, backtest it, then run it live — same code, same engine.

Supports **Alpaca** (stocks/ETFs), **TopStepX** (futures/prop trading), **Tradovate** (futures), **Interactive Brokers** (stocks/futures), **Coinbase** (crypto), and **Kalshi** (prediction markets).

---

## Quick Start

```bash
# Backtest an MNQ scalper against TopStepX historical data
go run ./cmd/backtest \
  --config configs/default.json \
  --data-provider topstepx \
  --symbols MNQ \
  --script scripts/mnq_scalp.js \
  --capital 50000

# Run it live
go run ./cmd/live \
  --config configs/default.json \
  --provider topstepx \
  --symbols MNQ \
  --script scripts/mnq_scalp.js
```

---

## Writing a Strategy

Strategies are JavaScript files. The engine calls your functions, you return orders.

```js
function timeframes() { return ["1m"] }

function onBar(timeframe, tick) {
  var sym = tick.symbol
  var pos = portfolio.position(sym)
  var spec = getContract(sym)

  // Already in a position? Brackets handle TP/SL.
  if (pos && pos.side !== "flat") return []

  // Entry signal
  var b = bars(sym, 30)
  if (b.length < 20) return []
  var closes = b.map(function(x) { return x.close })
  var rsi = ta.rsi(closes, 14)

  if (rsi < 30) {
    return [{ symbol: sym, side: "buy", qty: 1, orderType: "market",
              tpDistance: 2.0, slDistance: 1.0 }]
  }
  return []
}
```

### Globals (available in all functions)

| Global | Description |
|--------|-------------|
| `portfolio` | Current account state: `cash()`, `equity()`, `position(sym)`, `positions()`, `totalPL()`, `dailyPL()`, `dailyTrades()` |
| `bars(symbol, count)` | Last N bars from engine memory. Zero latency, no API calls. |
| `dailyLevels(symbol)` | `{prevHigh, prevLow, prevClose, todayHigh, todayLow, todayOpen}` |
| `getContract(symbol)` | `{symbol, tickSize, tickValue, pointValue}` — no hardcoding needed |
| `capital` | Initial capital (constant) |
| `config` | User key-value config from JSON |
| `ta` | Technical analysis: `sma`, `ema`, `rsi`, `macd`, `bollinger`, `atr`, `adx`, `stoch`, etc. |
| `data` | `data.history(symbol, bars, timeframe)` — fetches from provider API (use `bars()` when possible) |
| `ml` | Machine learning: `linearRegression`, `decisionTree`, `randomForest`, `knn`, `kmeans` |
| `console` | `console.log(...)` |
| `http` | `http.get(url)`, `http.post(url, body)` — HTTPS only |
| `searchAssets` | `searchAssets({text, assetClass})` — discover tradeable symbols |

### Position Object

```js
var pos = portfolio.position("MNQ")
// pos.qty          — positive for long, negative for short
// pos.side         — "long" or "short"
// pos.entryPrice   — actual fill price
// pos.avgCost      — average entry price
// pos.unrealizedPL — current unrealized P&L
// pos.holdingBars  — bars since position opened
// pos.marketValue  — current notional value
```

### Order Object

```js
{
  symbol:     "MNQ",        // required
  side:       "buy",        // required: "buy" or "sell"
  qty:        3,            // required
  orderType:  "market",     // "market" (default), "limit", "stop", "stop_limit"
  limitPrice: 24000,        // required for "limit" and "stop_limit"
  stopPrice:  23900,        // required for "stop" and "stop_limit"
  tpDistance: 1.50,         // take profit distance in price points (broker bracket)
  slDistance: 0.75,         // stop loss distance in price points (broker bracket)
  reason:    "RSI oversold" // optional, for logging
}
```

### Function Signatures

| Function | Parameters | Returns | Required |
|----------|-----------|---------|----------|
| `timeframes()` | none | `string[]` | Yes |
| `onBar(timeframe, tick)` | timeframe, tick | `order[]` | Yes |
| `onFill(fill)` | fill | void | No |
| `onTrade(trade)` | trade | `order[]` | No |
| `onMarketOpen()` | none | `order[]` | No |
| `onMarketClose()` | none | `order[]` | No |
| `onLive(ctx)` | ctx with positions | void | No |
| `onInit(ctx)` | ctx with symbols, config | void | No |
| `onExit()` | none | void | No |
| `resolveSymbols(ctx)` | ctx with searchAssets | `string[]` | No |

`portfolio` is a global — not passed as a parameter to any function.

### Bracket Orders

Set `tpDistance` and `slDistance` on entry orders. The broker places TP (limit) and SL (stop) orders automatically. When one fills, the other is cancelled.

```js
// Take profit 6 ticks from entry, stop loss 4 ticks
var spec = getContract(sym)
return [{ symbol: sym, side: "buy", qty: 3, orderType: "market",
          tpDistance: 6 * spec.tickSize, slDistance: 4 * spec.tickSize }]
```

Supported on TopStepX (native brackets), Alpaca (bracket orders), and Tradovate (OSO orders). Also simulated during backtesting.

---

## Configuration

Create a JSON config file with provider credentials and strategy parameters:

```json
{
  "topstepx": {
    "username": "you@example.com",
    "api_key": "your_key",
    "account_id": 12345678
  },
  "alpaca": {
    "api_key": "your_key",
    "secret": "your_secret",
    "base_url": "https://paper-api.alpaca.markets"
  },
  "strategy": {
    "MAX_CONTRACTS": "3",
    "TP_TICKS": "8",
    "SL_TICKS": "4",
    "DAILY_LOSS_LIMIT": "1000"
  }
}
```

Strategy parameters are available in scripts as `config.MAX_CONTRACTS`, etc.

---

## Running a Backtest

```bash
go run ./cmd/backtest \
  --config configs/default.json \
  --data-provider topstepx \
  --symbols MNQ \
  --script scripts/mnq_scalp.js \
  --capital 50000 \
  --from 2026-03-01 \
  --to 2026-03-26
```

| Flag | Default | Description |
|------|---------|-------------|
| `--script` | required | Path to JS strategy file |
| `--symbols` | `AAPL` | Comma-separated symbols |
| `--data-provider` | `alpaca` | `alpaca`, `topstepx`, `massive`, `coinbase`, `kalshi` |
| `--config` | none | JSON config file for credentials + strategy params |
| `--capital` | `10000` | Starting capital |
| `--from` | auto | Start date (YYYY-MM-DD) |
| `--to` | today | End date (YYYY-MM-DD) |

Results are saved to SQLite for review.

---

## Running Live

```bash
go run ./cmd/live \
  --config configs/default.json \
  --provider topstepx \
  --symbols MNQ \
  --script scripts/mnq_scalp.js
```

| Flag | Default | Description |
|------|---------|-------------|
| `--script` | required | Path to JS strategy file |
| `--symbols` | `AAPL` | Comma-separated symbols |
| `--provider` | `alpaca` | `alpaca`, `topstepx`, `tradovate`, `ibkr`, `coinbase`, `kalshi` |
| `--config` | none | JSON config file |
| `--capital` | `10000` | Starting capital (first run only — subsequent runs use broker state) |

On startup, the engine:
1. Fetches account balance and positions from the broker
2. Replays historical bars to warm up indicators
3. Calls `onLive()` to signal the transition to live trading
4. Begins streaming real-time bars and processing orders

Restarting mid-session is safe — the engine recovers from broker state.

---

## Providers

| Provider | Assets | Brackets | Notes |
|----------|--------|----------|-------|
| **TopStepX** | CME futures (ES, NQ, MES, MNQ, CL, GC...) | Native | Prop trading. 4:10 PM ET close. Min 4 tick bracket distance. |
| **Alpaca** | US stocks/ETFs | Native | Free tier available. IEX or SIP feeds. |
| **Tradovate** | CME futures | OSO orders | Cloud API, no local gateway. |
| **IBKR** | Stocks, futures | Not yet | Requires IB Gateway running locally. |
| **Coinbase** | Crypto (BTC, ETH, SOL...) | Not yet | 24/7 markets. |
| **Kalshi** | Prediction markets | N/A | Event-based pricing 0-1. |

---

## Project Structure

```
runbook/
├── cmd/
│   ├── backtest/          # CLI: run a backtest
│   ├── live/              # CLI: run live trading
│   └── topstepx-debug/    # CLI: inspect TopStepX account state
├── engine/                # Live trading engine (event loop, recovery, warmup)
├── backtest/              # Backtest engine (replay, bracket simulation)
├── strategy/              # Core interfaces (Strategy, Portfolio, Order, Fill)
├── strategies/script/     # JS runtime (goja VM, all globals, order parsing)
├── providers/
│   ├── alpaca/            # Alpaca stocks/ETFs
│   ├── topstepx/          # TopStepX futures (REST + SignalR)
│   ├── tradovate/         # Tradovate futures
│   ├── ibkr/              # Interactive Brokers
│   ├── coinbase/          # Coinbase crypto
│   └── kalshi/            # Kalshi prediction markets
├── provider/              # Shared interfaces (MarketData, Execution, ContractSpec)
├── internal/
│   ├── portfolio/         # Cash, positions, P&L tracking
│   ├── bracket/           # Shared TP/SL bracket simulation
│   ├── barbuf/            # Bar buffer + daily level tracker
│   └── db/                # SQLite logging
├── scripts/               # Example strategies
├── configs/               # Config files
└── designer.go            # AI strategy designer (used by server repo)
```

---

## Database

Results are logged to SQLite at `DATABASE_PATH` (default: `./trading_bot.db`).

| Table | Contents |
|-------|----------|
| `backtest_runs` | Summary metrics for each backtest |
| `backtest_fills` | Individual fills per run |
| `paper_orders` | Orders submitted during live trading (legacy table name) |
| `paper_fills` | Fill confirmations from broker (legacy table name) |
| `paper_snapshots` | Portfolio snapshots after each fill (legacy table name) |
