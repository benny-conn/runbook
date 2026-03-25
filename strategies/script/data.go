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
	historyTimeout         = 30 * time.Second
)

// Option configures optional features of a ScriptStrategy.
type Option func(*scriptOptions)

type scriptOptions struct {
	marketData provider.MarketData
	capital    float64
	hasCapital bool
	logSink   *LogSink
}

// LogSink captures console.log output from JS scripts during backtesting.
type LogSink struct {
	mu   sync.Mutex
	logs []string
}

const (
	maxLogEntries   = 50
	maxLogEntrySize = 500
)

// Add appends a log entry, truncating if necessary.
func (ls *LogSink) Add(msg string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if len(ls.logs) >= maxLogEntries {
		return
	}
	if len(msg) > maxLogEntrySize {
		msg = msg[:maxLogEntrySize] + "..."
	}
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

// WithLogSink captures console.log output into the provided sink instead of stdout.
func WithLogSink(sink *LogSink) Option {
	return func(o *scriptOptions) {
		o.logSink = sink
	}
}

// historyCacheKey uniquely identifies a cached history request.
type historyCacheKey struct {
	symbol    string
	bars      int
	timeframe string
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
}

func newDataGlobal(md provider.MarketData) *dataGlobal {
	return &dataGlobal{
		md:    md,
		cache: make(map[historyCacheKey]historyCacheEntry),
	}
}

// resetTickCount should be called at the start of each tick.
func (d *dataGlobal) resetTickCount() {
	d.mu.Lock()
	d.tickCallCount = 0
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

		// Check cache
		key := historyCacheKey{symbol: symbol, bars: bars, timeframe: timeframe}
		if entry, ok := dg.cache[key]; ok && time.Since(entry.fetchedAt) < historyCacheTTL {
			dg.mu.Unlock()
			return vm.ToValue(entry.data)
		}
		dg.mu.Unlock()

		// Compute time range: go far enough back to get `bars` bars.
		end := time.Now().UTC()
		var start time.Time
		switch timeframe {
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
			start = end.Add(-time.Duration(bars*2) * 24 * time.Hour)
		}

		ctx, cancel := context.WithTimeout(context.Background(), historyTimeout)
		defer cancel()

		fetchedBars, err := dg.md.FetchBars(ctx, symbol, timeframe, start, end)
		if err != nil {
			panic(vm.NewGoError(fmt.Errorf("data.history: %w", err)))
		}

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

		// Cache
		dg.mu.Lock()
		dg.cache[key] = historyCacheEntry{data: result, fetchedAt: time.Now()}
		dg.mu.Unlock()

		return vm.ToValue(result)
	})

	vm.Set("data", data)
}
