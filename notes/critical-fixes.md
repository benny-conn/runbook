1. Backtest multi-symbol order fills (backtest/engine.go)

- Added symbolTickIndices type with binary search to find the next tick for any symbol
- New fillOrdersCross method resolves per-symbol fill prices, so a strategy processing AAPL can correctly place orders for MSFT/GOOG
- Added TestEngine_CrossSymbolOrders test to verify

2. OnFill errors silently swallowed (strategies/script/script.go:323-338)

- Changed from discarding the return value to checking err and calling trackError() + fmt.Println(), matching OnBar's error handling pattern

3. Recovery warmup UTC session boundaries (engine/recovery.go)

- Added marketDateLocation() helper that uses the configured market schedule timezone (falls back to America/New_York then UTC)
- Recovery warmup now uses market-timezone dates instead of UTC for day boundary detection

4. IBKR reconnection logic (providers/ibkr/tws.go)

- Added connect() method extracted from newTWS() for reuse
- ic.Run() now triggers reconnectLoop() when it returns (connection drops)
- Exponential backoff: 1s, 2s, 5s, 10s, 30s between attempts
- Connection health tracking via connAlive atomic bool
- TWS error codes 1100-1102 (connectivity lost/restored) now update connection state

5. Coinbase quote timestamps (providers/coinbase/coinbase.go)

- SubscribeQuotes now parses the envelope's RFC3339 timestamp from the exchange instead of using time.Now()
- Falls back to local clock only if parsing fails

6. Event channel fill overflow (engine/engine.go)

- Added dedicated fillCh channel (buffered, capacity 64) separate from the general event channel
- Fill subscription writes to fillCh with blocking semantics (never drops)
- processLoop prioritizes fills over other events using a two-stage select
