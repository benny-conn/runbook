#17. float64 precision documented (internal/portfolio/portfolio.go) — Added documentation explaining the float64 precision tradeoffs and when it matters.

#18. Config validation at startup (providers/alpaca/, providers/tradovate/, providers/coinbase/) — Added WARNING log messages at construction time when required  
 credentials (API keys, secrets, passwords) are missing. Catches typos and missing env vars before the first API call.

#19. Multi-timeframe aggregator bar ordering (backtest/engine.go) — Added explicit sort.Slice on completed bars by timeframe duration (shortest first) when multiple
aggregators emit on the same tick. Guarantees strategies always see 5m before 1h, matching live engine behavior.

#20. Warmup limit order limitation documented (engine/recovery.go) — Expanded the simulateFills comment to explicitly document the limitation and recommend  
 workarounds (use market orders or implement PositionSeeder).

#21. ML configurable random seed (strategies/script/ml.go) — randomForest and kmeans now accept an optional seed parameter (default 42 for backward compat). Set  
 seed: 0 for time-based non-deterministic seed.

#22. Tradovate token renewal logging (providers/tradovate/auth.go) — Token renewal failures now log the error, and re-authentication success/failure is also logged.
Previously both were completely silent.

#23 & #24 (rate limit coordination, test coverage) — Skipped as they require larger architectural changes beyond a single commit scope.

Also allowed the backend to supply positions (like if they stored orders in the DB) and cash optionally:

```go
eng := engine.NewEngine(strat, md, exec, store, engine.Config{
 Capital: 10000,
 WarmupBars: 100,

      // Supply the positions this specific strategy had when it last stopped.
      // Retrieved from your database (paper_fills/paper_snapshots).
      Positions: []provider.Position{
          {Symbol: "MNQH5", Qty: 2, AvgEntryPrice: 19850.50, CurrentPrice: 19870.00},
      },

      // Optionally override cash too (e.g. the strategy's allocated capital minus cost basis).
      CashOverride: 8500.00,

})
```

The key behavior:

- Positions is nil (default) → queries the broker as before, no change
- Positions is []provider.Position{} (empty slice, non-nil) → tells the engine "this strategy has no open positions", even if the broker account does
- Positions has entries → uses exactly those positions, ignoring the broker

This means the backend can persist each strategy's fills in its database, compute the current open positions at restart time, and feed them in. The warmup replay  
 still runs to rebuild indicator state, and PositionSeeder still fires to inject positions into the JS strategy's internal tracking.
