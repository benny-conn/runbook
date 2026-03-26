package provider

import (
	"context"
	"sort"
	"sync"
	"time"
)

// PrefetchMarketData wraps a MarketData provider and serves FetchBars calls
// from pre-loaded data. Designed for backtesting where the full date range is
// known upfront, avoiding hundreds of per-bar API calls from data.history().
//
// For timeframe+symbol combinations that weren't prefetched, it falls through
// to the underlying provider (with per-symbol caching to avoid repeated calls).
type PrefetchMarketData struct {
	inner MarketData

	mu        sync.RWMutex
	prefetched map[prefetchKey][]Bar // symbol+timeframe → sorted bars
	// fallback cache for timeframes not prefetched (e.g. strategy uses "15s"
	// but data.history() asks for "1m" for indicator warmup).
	fallback map[prefetchKey][]Bar
}

type prefetchKey struct {
	symbol    string
	timeframe string
}

// NewPrefetchMarketData creates a wrapper from already-fetched bars. The bars
// are indexed by symbol and timeframe for fast FetchBars lookups. For timeframes
// not present in the prefetched data, calls fall through to the underlying
// provider with caching.
func NewPrefetchMarketData(inner MarketData, bars []Bar, timeframe string) *PrefetchMarketData {
	p := &PrefetchMarketData{
		inner:      inner,
		prefetched: make(map[prefetchKey][]Bar),
		fallback:   make(map[prefetchKey][]Bar),
	}

	// Index by symbol.
	for _, b := range bars {
		key := prefetchKey{symbol: b.Symbol, timeframe: timeframe}
		p.prefetched[key] = append(p.prefetched[key], b)
	}

	// Ensure each symbol's bars are sorted by timestamp.
	for key, bars := range p.prefetched {
		sort.Slice(bars, func(i, j int) bool {
			return bars[i].Timestamp.Before(bars[j].Timestamp)
		})
		p.prefetched[key] = bars
	}

	return p
}

// FetchBars returns bars from the prefetched data if available, otherwise
// falls through to the underlying provider with caching.
func (p *PrefetchMarketData) FetchBars(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]Bar, error) {
	key := prefetchKey{symbol: symbol, timeframe: timeframe}

	// Try prefetched data first.
	p.mu.RLock()
	if bars, ok := p.prefetched[key]; ok {
		p.mu.RUnlock()
		return sliceBars(bars, start, end), nil
	}
	// Try fallback cache.
	if bars, ok := p.fallback[key]; ok {
		p.mu.RUnlock()
		return sliceBars(bars, start, end), nil
	}
	p.mu.RUnlock()

	// Not prefetched — fetch from the underlying provider and cache.
	// Use a wide range to maximize cache hits for future calls.
	bars, err := p.inner.FetchBars(ctx, symbol, timeframe, start, end)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	existing, ok := p.fallback[key]
	if ok {
		// Merge with existing cached bars.
		bars = mergeBars(existing, bars)
	}
	sort.Slice(bars, func(i, j int) bool {
		return bars[i].Timestamp.Before(bars[j].Timestamp)
	})
	p.fallback[key] = bars
	p.mu.Unlock()

	return sliceBars(bars, start, end), nil
}

// FetchBarsMulti delegates to the underlying provider.
func (p *PrefetchMarketData) FetchBarsMulti(ctx context.Context, symbols []string, timeframe string, start, end time.Time) ([]Bar, error) {
	return p.inner.FetchBarsMulti(ctx, symbols, timeframe, start, end)
}

// SubscribeBars delegates to the underlying provider.
func (p *PrefetchMarketData) SubscribeBars(ctx context.Context, symbols []string, timeframe string, handler func(Bar)) error {
	return p.inner.SubscribeBars(ctx, symbols, timeframe, handler)
}

// SubscribeTrades delegates to the underlying provider.
func (p *PrefetchMarketData) SubscribeTrades(ctx context.Context, symbols []string, handler func(Trade)) error {
	return p.inner.SubscribeTrades(ctx, symbols, handler)
}

// SubscribeQuotes delegates to the underlying provider.
func (p *PrefetchMarketData) SubscribeQuotes(ctx context.Context, symbols []string, handler func(Quote)) error {
	return p.inner.SubscribeQuotes(ctx, symbols, handler)
}

// sliceBars returns the subset of sorted bars within [start, end].
func sliceBars(bars []Bar, start, end time.Time) []Bar {
	// Binary search for the first bar >= start.
	lo := sort.Search(len(bars), func(i int) bool {
		return !bars[i].Timestamp.Before(start)
	})
	// Binary search for the first bar > end.
	hi := sort.Search(len(bars), func(i int) bool {
		return bars[i].Timestamp.After(end)
	})
	if lo >= hi {
		return nil
	}
	result := make([]Bar, hi-lo)
	copy(result, bars[lo:hi])
	return result
}

// mergeBars combines two bar slices, deduplicating by timestamp.
func mergeBars(a, b []Bar) []Bar {
	seen := make(map[int64]struct{}, len(a))
	for _, bar := range a {
		seen[bar.Timestamp.UnixNano()] = struct{}{}
	}
	merged := make([]Bar, len(a), len(a)+len(b))
	copy(merged, a)
	for _, bar := range b {
		if _, ok := seen[bar.Timestamp.UnixNano()]; !ok {
			merged = append(merged, bar)
		}
	}
	return merged
}
