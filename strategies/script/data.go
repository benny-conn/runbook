package script

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/benny-conn/brandon-bot/provider"
	"github.com/dop251/goja"
)

const (
	maxHistoryCallsPerTick = 10
	historyCacheTTL        = 5 * time.Minute
	historyTimeout         = 5 * time.Second
	fetchRetries           = 2
	fetchRetryBaseDelay    = 500 * time.Millisecond
	fetchErrorCooldown     = 10 * time.Second // suppress retries after an error
	maxCacheEntries        = 500              // cap to prevent unbounded memory growth
	cacheEvictInterval     = 50               // evict stale entries every N cache writes
)

// Option configures optional features of a ScriptStrategy.
type Option func(*scriptOptions)

type scriptOptions struct {
	marketData      provider.MarketData
	capital         float64
	hasCapital      bool
	logSink         *LogSink
	callbackTimeout time.Duration // max time a hot-path JS callback can run (0 = 5s default)
}

// LogSink captures console.log output from JS scripts.
type LogSink struct {
	mu   sync.Mutex
	logs []string
}

// Add appends a log entry.
func (ls *LogSink) Add(msg string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.logs = append(ls.logs, msg)
}

// Logs returns a copy of all captured log entries.
func (ls *LogSink) Logs() []string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	out := make([]string, len(ls.logs))
	copy(out, ls.logs)
	return out
}

// Drain returns all captured log entries and clears the internal buffer.
func (ls *LogSink) Drain() []string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	out := ls.logs
	ls.logs = nil
	return out
}

// WithMarketData injects a MarketData provider, enabling the `data` global.
func WithMarketData(md provider.MarketData) Option {
	return func(o *scriptOptions) {
		o.marketData = md
	}
}

// WithCapital injects the initial capital as a read-only `capital` global.
func WithCapital(c float64) Option {
	return func(o *scriptOptions) {
		o.capital = c
		o.hasCapital = true
	}
}

// WithCallbackTimeout sets the maximum duration a hot-path JS callback (onBar,
// onTrade, onFill, onMarketOpen, onMarketClose) can run before being interrupted.
// Default is 5 seconds. This protects against infinite loops in scripts hanging
// the engine. Init-time callbacks (onInit, resolveSymbols, onExit) are not subject
// to this timeout.
func WithCallbackTimeout(d time.Duration) Option {
	return func(o *scriptOptions) {
		o.callbackTimeout = d
	}
}

// WithLogSink captures console.log output into the provided sink instead of stdout.
func WithLogSink(sink *LogSink) Option {
	return func(o *scriptOptions) {
		o.logSink = sink
	}
}

// historyCacheKey uniquely identifies a cached history request.
// Includes the truncated end time so cache entries are invalidated when
// the simulated bar time advances during backtesting.
type historyCacheKey struct {
	symbol    string
	bars      int
	timeframe string
	endTime   int64 // Unix seconds, truncated to cache granularity
}

type historyCacheEntry struct {
	data      []interface{}
	fetchedAt time.Time
}

// dataGlobal tracks rate limiting and caching for the `data` global.
type dataGlobal struct {
	md    provider.MarketData
	mu    sync.Mutex
	cache map[historyCacheKey]historyCacheEntry
	// tickCallCount resets each tick; enforced by the strategy
	tickCallCount int
	// cacheWrites counts cache insertions to trigger periodic eviction.
	cacheWrites int
	// currentTime is the simulated bar timestamp, updated each tick.
	// Used instead of time.Now() so data.history() works correctly during backtesting.
	currentTime time.Time
	// lastFetchError tracks the last fetch error time per symbol to implement
	// a cooldown period that prevents hammering a rate-limited API.
	lastFetchError map[string]time.Time
}

func newDataGlobal(md provider.MarketData) *dataGlobal {
	return &dataGlobal{
		md:             md,
		cache:          make(map[historyCacheKey]historyCacheEntry),
		lastFetchError: make(map[string]time.Time),
	}
}

// resetTick should be called at the start of each tick to reset rate limits
// and update the current simulated time for data.history() calls.
func (d *dataGlobal) resetTick(barTime time.Time) {
	d.mu.Lock()
	d.tickCallCount = 0
	d.currentTime = barTime
	d.mu.Unlock()
}

// registerData injects the `data` global object into the Goja VM.
func registerData(vm *goja.Runtime, dg *dataGlobal) {
	data := vm.NewObject()

	// data.history(symbol, bars, timeframe) → array of { timestamp, open, high, low, close, volume }
	data.Set("history", func(call goja.FunctionCall) goja.Value {
		symbol := call.Argument(0).String()
		bars := toInt(call.Argument(1).Export())
		timeframe := call.Argument(2).String()

		if symbol == "" || bars <= 0 || timeframe == "" {
			panic(vm.NewGoError(fmt.Errorf("data.history: invalid arguments")))
		}

		dg.mu.Lock()
		dg.tickCallCount++
		if dg.tickCallCount > maxHistoryCallsPerTick {
			dg.mu.Unlock()
			panic(vm.NewGoError(fmt.Errorf("data.history: rate limit exceeded (%d calls per tick)", maxHistoryCallsPerTick)))
		}

		// Use the current bar timestamp (set by resetTick) so data.history()
		// returns data relative to the simulated time during backtesting.
		// Falls back to time.Now() for live trading (before first tick).
		end := dg.currentTime.UTC()
		if end.IsZero() {
			end = time.Now().UTC()
		}

		// Cache key includes truncated end time so advancing bar time
		// eventually invalidates entries. Granularity is intentionally coarse
		// (5-10x the bar duration) to avoid hammering the API — a script
		// calling data.history(sym, 50, "1m") at 10:01 and 10:05 gets
		// nearly identical data (49/50 bars overlap). During backtesting
		// this is critical because simulated time advances rapidly.
		var cacheGranularity time.Duration
		switch timeframe {
		case "1s":
			cacheGranularity = 30 * time.Second
		case "15s":
			cacheGranularity = 2 * time.Minute
		case "30s":
			cacheGranularity = 3 * time.Minute
		case "1m":
			cacheGranularity = 5 * time.Minute
		case "5m":
			cacheGranularity = 15 * time.Minute
		case "15m":
			cacheGranularity = 30 * time.Minute
		case "30m":
			cacheGranularity = 30 * time.Minute
		case "1h":
			cacheGranularity = 2 * time.Hour
		case "1d":
			cacheGranularity = 24 * time.Hour
		default:
			cacheGranularity = 15 * time.Minute
		}
		truncatedEnd := end.Truncate(cacheGranularity)
		key := historyCacheKey{symbol: symbol, bars: bars, timeframe: timeframe, endTime: truncatedEnd.Unix()}
		if entry, ok := dg.cache[key]; ok && time.Since(entry.fetchedAt) < historyCacheTTL {
			dg.mu.Unlock()
			return vm.ToValue(entry.data)
		}
		// Check if this symbol is in a cooldown period after a recent error.
		if lastErr, ok := dg.lastFetchError[symbol]; ok && time.Since(lastErr) < fetchErrorCooldown {
			dg.mu.Unlock()
			// Return empty array during cooldown instead of hammering the API.
			return vm.ToValue([]interface{}{})
		}
		dg.mu.Unlock()

		var start time.Time
		switch timeframe {
		case "1s":
			start = end.Add(-time.Duration(bars*2) * time.Second)
		case "15s":
			start = end.Add(-time.Duration(bars*30) * time.Second)
		case "30s":
			start = end.Add(-time.Duration(bars*60) * time.Second)
		case "1m":
			start = end.Add(-time.Duration(bars*2) * time.Minute)
		case "5m":
			start = end.Add(-time.Duration(bars*10) * time.Minute)
		case "15m":
			start = end.Add(-time.Duration(bars*30) * time.Minute)
		case "30m":
			start = end.Add(-time.Duration(bars*60) * time.Minute)
		case "1h":
			start = end.Add(-time.Duration(bars*2) * time.Hour)
		case "1d":
			start = end.Add(-time.Duration(bars*2) * 24 * time.Hour)
		default:
			start = end.Add(-time.Duration(bars*2) * time.Minute) // conservative default
		}

		// Fetch with retry and exponential backoff for transient errors (e.g. 429).
		var fetchedBars []provider.Bar
		var fetchErr error
		for attempt := 0; attempt <= fetchRetries; attempt++ {
			if attempt > 0 {
				time.Sleep(fetchRetryBaseDelay * time.Duration(1<<(attempt-1)))
			}
			ctx, cancel := context.WithTimeout(context.Background(), historyTimeout)
			fetchedBars, fetchErr = dg.md.FetchBars(ctx, symbol, timeframe, start, end)
			cancel()
			if fetchErr == nil {
				break
			}
		}
		if fetchErr != nil {
			// Record the error time so subsequent ticks use cooldown.
			dg.mu.Lock()
			dg.lastFetchError[symbol] = time.Now()
			dg.mu.Unlock()
			panic(vm.NewGoError(fmt.Errorf("data.history: %w", fetchErr)))
		}
		// Clear any previous error cooldown on success.
		dg.mu.Lock()
		delete(dg.lastFetchError, symbol)
		dg.mu.Unlock()

		// Trim to requested count (take last N bars)
		if len(fetchedBars) > bars {
			fetchedBars = fetchedBars[len(fetchedBars)-bars:]
		}

		// Convert to JS-friendly format
		result := make([]interface{}, len(fetchedBars))
		for i, b := range fetchedBars {
			result[i] = map[string]interface{}{
				"timestamp": b.Timestamp.UnixMilli(),
				"open":      b.Open,
				"high":      b.High,
				"low":       b.Low,
				"close":     b.Close,
				"volume":    b.Volume,
			}
		}

		// Cache with periodic eviction to bound memory usage.
		dg.mu.Lock()
		dg.cache[key] = historyCacheEntry{data: result, fetchedAt: time.Now()}
		dg.cacheWrites++
		if dg.cacheWrites%cacheEvictInterval == 0 || len(dg.cache) > maxCacheEntries {
			now := time.Now()
			for k, v := range dg.cache {
				if now.Sub(v.fetchedAt) > historyCacheTTL {
					delete(dg.cache, k)
				}
			}
			// If still over limit after TTL eviction, drop oldest entries.
			for len(dg.cache) > maxCacheEntries {
				var oldestKey historyCacheKey
				var oldestTime time.Time
				for k, v := range dg.cache {
					if oldestTime.IsZero() || v.fetchedAt.Before(oldestTime) {
						oldestKey = k
						oldestTime = v.fetchedAt
					}
				}
				delete(dg.cache, oldestKey)
			}
		}
		dg.mu.Unlock()

		return vm.ToValue(result)
	})

	vm.Set("data", data)
}
