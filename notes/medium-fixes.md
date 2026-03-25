#7. Timeframes() returns safe default on error (strategies/script/script.go) — Returns ["1m"] instead of nil when JS timeframes() throws or returns non-array.

#8. data.history() cache granularity (strategies/script/data.go) — Added missing "1h" case (2h granularity) and fixed "1d" from 15min to 24h.

#9. Backtest equity curve reflects EOD orders (backtest/engine.go) — Moved recordEquity() to AFTER OnMarketClose fills are applied, both at day boundaries and at the
final close.

#10. Volume rounding in BarToTick (provider/provider.go) — Changed int64(b.Volume) to int64(math.Round(b.Volume)) to round instead of truncate fractional volumes.

#11. Bounded data.history() cache (strategies/script/data.go) — Added maxCacheEntries=500 cap with periodic eviction (every 50 writes). Evicts expired entries first,
then oldest if still over limit.

#12. data.history() timeout reduced (strategies/script/data.go) — Reduced historyTimeout from 30s to 5s. This limits how long s.mu is held during network calls,  
 preventing fill events from queuing up behind slow API responses.

#13. TopstepX quote delta validation (providers/topstepx/topstepx.go) — SubscribeQuotes now skips emitting quotes until both bid and ask are non-zero, preventing  
 incomplete state from reaching strategies.

#14. TopstepX bar timestamps (providers/topstepx/topstepx.go) — Uses interval-aligned bar time instead of last quote timestamp when the quote timestamp is stale  
 (older than 2x the interval). Falls back to aligned time.Now().Truncate(interval).

Issues #15 (Kalshi polling) and #16 (stop reentrancy) were left as-is — #15 is an optimization that needs a bigger refactor, and #16 is already mitigated by the  
 single-goroutine event loop.
