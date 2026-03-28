Full Audit: runbook

Critical Issues (High Impact, Should Fix)

1. Backtest multi-symbol order fills are broken  


backtest/engine.go:340-364 — When a strategy returns orders for a symbol OTHER than the current tick's symbol, the order attempts to fill using the wrong symbol's  
 price. For example, if processing an AAPL tick and the strategy returns an order for MSFT, it tries to fill MSFT at AAPL's next open price. This silently fails with
"no price data available." The live engine doesn't have this problem since orders go directly to the broker.

Fix: Track next-tick indices per symbol, not just the current symbol. Build a map[symbol]nextBarIndex so cross-symbol orders fill at the correct price.

2. OnFill errors silently swallowed  


strategies/script/script.go:323-338 — If the JS onFill() function throws an error, it's completely ignored. Compare to OnBar (line 289) which logs errors and calls  
 trackError(). This means a bug in a user's onFill() handler is invisible — the strategy will silently stop processing fills correctly while continuing to trade.

Fix: Add error handling matching OnBar's pattern: log + trackError().

3. Recovery warmup uses UTC for session boundaries, not market time  


engine/recovery.go:86 — Day boundary detection during warmup uses tick.Timestamp.UTC().Format("2006-01-02"). For futures (ES, MNQ), the trading day doesn't align  
 with UTC midnight. OnMarketClose/OnMarketOpen will fire at the wrong times during warmup, corrupting daily P&L tracking and any session-boundary strategy logic.

Fix: Use market-timezone-aware session boundaries (the live engine already does this correctly at engine.go:440-497).

4. IBKR has no reconnection logic  


providers/ibkr/ — The IBKR provider connects to a local TWS socket but has no heartbeat, no reconnection, and no error recovery. If TWS restarts or the socket drops,
the provider silently dies. For a provider targeting live futures trading, this is a gap.

5. Coinbase quote/bar timestamps use local clock

providers/coinbase/coinbase.go — Quote handler sets Timestamp: time.Now() instead of the exchange timestamp. Bars built from trade aggregation also use local time.  
 Any latency-sensitive strategy will see incorrect timing — potentially seconds off from actual market events.

6. Event channel silent overflow

engine/engine.go:349 — The event channel is fixed at 512 slots. If the processLoop is slow (e.g., JS strategy calling data.history() with a network fetch), events  
 are silently dropped with a log message. Dropped fills mean the portfolio state diverges from broker state.

Fix: At minimum, fills should never be dropped — use a separate unbuffered or large-capacity fill channel with backpressure.

---

Medium Issues (Correctness/Consistency Gaps)

7. Timeframes() returns nil on JS error instead of safe default

strategies/script/script.go:255-285 — If the JS timeframes() function throws, the method returns nil instead of []string{"1m"} (which is the default when the  
 function doesn't exist). Downstream code expects a non-nil slice.

8. data.history() cache granularity bugs

strategies/script/data.go:169-187 — Two wrong values:

- "1h" timeframe: missing from switch, falls through to default 15-min granularity (should be ~1h)
- "1d" timeframe: uses 15-min granularity (should be 24h+)  


This means 1h bars get evicted from cache too aggressively, and 1d bars are also cached incorrectly.

9. Backtest equity curve doesn't reflect EOD orders  


backtest/engine.go:312 — Equity is recorded BEFORE OnMarketClose fires, so any end-of-day orders (close all positions, etc.) aren't reflected in the equity curve or
max drawdown calculations.

10. Volume type truncation in BarToTick

provider/provider.go:169 — Volume is cast from float64 (provider.Bar) to int64 (strategy.Tick). Fractional volumes are silently truncated. Similarly,  
 strategy.Trade.Size is uint32 while provider.Trade.Size is float64 — truncated at conversion.

11. data.history() cache is unbounded

strategies/script/data.go:106 — The cache map grows indefinitely. On long backtests (1 year of 1m bars with data.history() calls), this can consume significant  
 memory. There's a 5-minute TTL but no proactive eviction — stale entries persist until re-accessed.

12. s.mu held during network calls in OnBar

strategies/script/script.go:289-319 — The strategy mutex is held for the entire duration of OnBar, including any data.history() network calls (up to 30 seconds).  
 This blocks all other callbacks (OnFill, OnTrade, OnQuote) from being processed, potentially causing fill events to queue up behind a slow data fetch.

13. TopstepX quote delta merging fragility

providers/topstepx/ — Quote delta fields are pointers (nil = absent). If the exchange sends an incomplete delta, old state is retained without validation. No check  
 that the merged state is complete before emitting.

14. TopstepX/Tradovate bar timestamps can be stale

Both providers build bars from quote/trade events. If no trades occur during a bar interval, the timestamp comes from the last quote (which could be from the  
 previous interval) or falls back to time.Now().

15. Kalshi bar subscription polls REST instead of using WebSocket push

providers/kalshi/kalshi.go — SubscribeBars continuously polls the candlestick REST endpoint on a timer instead of aggregating from the WebSocket trade stream. This  
 adds latency and consumes rate limit budget.

16. Client-side stop reentrancy

engine/engine.go:536-561 — checkStops() is called from bar, trade, AND quote events. If the same price event triggers from multiple paths in rapid succession, a stop
could theoretically be triggered more than once before removal (though the current single-goroutine design mitigates this).

---

Low Priority / Design Considerations

17. float64 used for all financial calculations

No decimal/fixed-point library is used for P&L, position cost basis, or equity calculations. For high-frequency small fills, rounding errors accumulate. Not a  
 showstopper but worth documenting for users.

18. Config validation is missing everywhere

All providers silently accept invalid/missing config keys. Typos in JSON config keys are ignored, and missing credentials aren't caught until the first API call. A  
 validation pass at startup would save users debugging time.

19. Multi-timeframe aggregator bar ordering

When multiple aggregators emit bars on the same tick (e.g., 5m and 1h both complete), they're processed in array order, not by timeframe hierarchy. If a strategy  
 depends on "process 1m before 5m before 1h," the behavior may differ between backtest and live.

20. Warmup only simulates market orders

engine/recovery.go:162-182 — During recovery warmup, only market orders are simulated. If a strategy uses limit orders for entries, those fills won't be replicated  
 during warmup, so indicator state and position tracking may be incorrect after recovery.

21. ML random seed is fixed

strategies/script/ml.go:166 — RandomForest uses seed=42 always. Backtests are deterministic but don't explore bootstrap randomness. Not a bug, but worth noting for  
 users relying on ML-based strategies.

22. Tradovate token renewal failure is silent

providers/tradovate/auth.go — If token renewal fails, it silently falls back to full re-authentication with no error surfaced to the caller.

23. No global rate limit coordination across strategies  


Multiple strategies using the same provider share API rate limits but have no coordination mechanism.

24. Test coverage gaps  


Well-tested: portfolio, database, aggregator, risk, backtest engine. Under-tested: provider type conversions, timezone handling, config validation, decimal precision
edge cases, script runtime error boundaries. Not tested: concurrent aggregator access, provider failover, DST transitions.

---

Architecture Assessment

What's working well:

- Clean provider abstraction (MarketData + Execution interfaces with optional capabilities)
- Single-goroutine event loop in the engine eliminates most concurrency bugs
- JS strategy runtime is well-designed with good global injection
- Portfolio tracking with futures multiplier support is solid and well-tested
- Multi-timeframe aggregation is correct
- Recovery/warmup system is a thoughtful design for server restarts  


What feels fragile:

- The gap between backtest and live engine behavior (especially multi-symbol fills, session boundaries, error handling) means a strategy that backtests well may  
  behave differently live
- Silent failures throughout (dropped events, swallowed errors, missing config validation) make debugging hard
- Provider implementations vary significantly in robustness (Kalshi/TopstepX are thorough; IBKR is minimal)
- The data.history() path holding the strategy lock during network calls is a latency footgun  


Recommended priority order for fixes:

1. Issues #1-2 (backtest accuracy + error visibility) — directly impacts end users' trust in backtest results
2. Issue #6 (fill channel overflow) — data loss in production
3. Issue #3 (recovery timezone) — breaks futures daily tracking
4. Issues #7-8 (nil timeframes + cache bugs) — easy wins
5. Issue #12 (lock during network calls) — architectural, harder to fix but important for reliability
