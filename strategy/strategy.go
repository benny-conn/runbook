package strategy

import (
	"context"
	"time"
)

// Tick represents a single OHLCV bar for a symbol.
type Tick struct {
	Symbol    string
	Timestamp time.Time
	Open      float64
	High      float64
	Low       float64
	Close     float64
	Volume    int64
}

// Order is a trade instruction returned by a strategy.
//
// Contract per OrderType:
//
//	"market"     — requires: symbol, side, qty. Optional: tpDistance, slDistance.
//	"limit"      — requires: symbol, side, qty, limitPrice. Optional: tpDistance, slDistance.
//	"stop"       — requires: symbol, side, qty, stopPrice. No brackets.
//	"stop_limit" — requires: symbol, side, qty, stopPrice, limitPrice. No brackets.
type Order struct {
	Symbol     string
	Side       string  // "buy" or "sell"
	Qty        float64
	OrderType  string  // "market", "limit", "stop", or "stop_limit"
	LimitPrice float64 // required for "limit" and "stop_limit" orders
	StopPrice  float64 // required for "stop" and "stop_limit" orders
	TPDistance  float64 // take profit distance in price points from entry (0 = none)
	SLDistance  float64 // stop loss distance in price points from entry (0 = none)
	Reason     string  // for logging/debugging
}

// Fill is the result of an executed order.
type Fill struct {
	Symbol     string
	Side       string  // "buy", "sell", or "short" (opening a new short)
	Qty        float64
	Price      float64
	Timestamp  time.Time
	RealizedPL float64 // per-fill realized P&L (set by engine before logging)
}

// Position represents an open holding in the portfolio.
type Position struct {
	Symbol       string
	Qty          float64
	AvgCost      float64
	MarketValue  float64
	UnrealizedPL float64
	EntryPrice   float64 // actual fill price (same as AvgCost for single-fill entries)
	Side         string  // "long", "short", or "flat"
	HoldingBars  int     // number of bars since position was opened
}

// Portfolio is a read-only view of the current account state.
// The engine updates the portfolio global before each strategy callback.
type Portfolio interface {
	Cash() float64
	Equity() float64
	Position(symbol string) *Position
	Positions() []Position
	TotalPL() float64
	DailyPL() float64    // realized + unrealized P&L since last daily reset
	DailyTrades() int    // completed round-trips since last daily reset
}

// Trade represents a single real-time trade print from the exchange.
type Trade struct {
	Symbol     string
	Timestamp  time.Time
	Price      float64
	Size       uint32
	Exchange   string
	Conditions []string
}

// Strategy is implemented by any trading algorithm.
//
// Timeframes returns the bar resolutions the strategy needs (e.g. ["1m"],
// ["1m","5m","1h"]). The engine subscribes to the finest timeframe from the
// list and aggregates higher-timeframe bars locally. This is the single source
// of truth for what timeframes are used — it is not configurable via CLI flags.
//
// OnBar is called for every completed bar at every declared timeframe.
// The timeframe parameter indicates which resolution fired (e.g. "1m", "5m").
// For single-timeframe strategies, it will always be the one declared timeframe.
type Strategy interface {
	Name() string
	Timeframes() []string
	SetPortfolio(portfolio Portfolio) // called by engine before each callback
	OnBar(timeframe string, tick Tick) []Order
	OnFill(fill Fill)
}

// TradeSubscriber is an optional interface a strategy can implement to receive
// individual trade prints instead of (or in addition to) completed bars.
// If the strategy implements this, the paper engine will also subscribe to
// the trade stream for the requested symbols.
type TradeSubscriber interface {
	OnTrade(trade Trade) []Order
}

// Quote represents a real-time bid/ask update from the exchange.
type Quote struct {
	Symbol    string
	Timestamp time.Time
	BidPrice  float64
	BidSize   float64
	AskPrice  float64
	AskSize   float64
}

// QuoteSubscriber is an optional interface a strategy can implement to receive
// real-time bid/ask quote updates. If implemented, the engine subscribes to the
// quote stream for the requested symbols in addition to bars.
// Useful for spread-aware entry logic and limit order placement.
type QuoteSubscriber interface {
	OnQuote(quote Quote) []Order
}

// InitContext holds the context passed to a strategy's OnInit hook and
// SymbolResolver.ResolveSymbols.
type InitContext struct {
	Symbols   []string
	Timeframe string
	Config    []byte // raw JSON config (nil if none)

	// Search provides asset/symbol discovery if the provider supports it.
	// Nil if the provider doesn't implement AssetSearch.
	Search AssetSearch

	// Discovery provides market listing capabilities if the provider supports it.
	// Deprecated: Use Search instead. Kept for backward compatibility.
	Discovery MarketDiscovery

	// AddSymbols dynamically subscribes to new symbols mid-run. Call from OnBar,
	// OnFill, etc. to start streaming data for new symbols without restarting.
	// Nil during backtest or before the live stream starts.
	AddSymbols func(symbols ...string)
}

// Initializer is an optional interface a strategy can implement to run setup
// logic once before any market data arrives. Use it to fetch news, run AI
// analysis, build watchlists, or load historical data. No per-tick overhead
// and since it runs before the data stream starts, the timeout constraints
// are relaxed (you could give it a longer timeout).
type Initializer interface {
	OnInit(ctx InitContext) error
}

// DailySessionHandler is an optional interface a strategy can implement to
// receive callbacks at market open and/or close each day.
// OnMarketOpen fires once at market open — useful for gap analysis,
// pre-market movers, setting daily levels. Can return orders.
// OnMarketClose fires once at market close — useful for EOD cleanup,
// flattening positions, logging daily P&L, preparing for the next day.
type DailySessionHandler interface {
	OnMarketOpen() []Order
	OnMarketClose() []Order
}

// Shutdowner is an optional interface a strategy can implement to run
// cleanup logic when the strategy is stopped (e.g. final logging,
// releasing resources).
type Shutdowner interface {
	OnExit()
}

// SymbolResolver is an optional interface a strategy can implement to
// dynamically determine which symbols to trade. Called once during engine
// startup, before OnInit and before any subscriptions. The returned symbols
// are merged with any CLI-provided symbols. If the strategy doesn't implement
// this, only CLI symbols are used.
type SymbolResolver interface {
	ResolveSymbols(ctx InitContext) ([]string, error)
}

// PositionSeeder is an optional interface a strategy can implement to accept
// position state injected during warm-up recovery. If a strategy doesn't
// implement this, position reconciliation is skipped (indicator warm-up still runs).
type PositionSeeder interface {
	SeedPosition(symbol string, qty, avgCost float64)
}

// LiveContext is passed to LiveHandler.OnLive when the engine transitions
// from warmup to live trading.
type LiveContext struct {
	Positions []Position // current broker positions at go-live time
}

// LiveHandler is an optional interface a strategy can implement to receive
// a one-time callback when live trading begins (after warmup replay and
// portfolio reset). Use this to clear warmup artifacts (halted flags, daily
// P&L) and inspect the real broker state before the first live bar.
type LiveHandler interface {
	OnLive(ctx LiveContext)
}

// RuntimeErrorReporter is an optional interface a strategy can implement to
// expose runtime errors for status logging. ScriptStrategy implements this.
type RuntimeErrorReporter interface {
	RuntimeErrors() []string
}

// BarBuffer provides access to recent bar history. Implemented by barbuf.Buffer.
type BarBuffer interface {
	Last(symbol string, n int) []Tick
}

// DailyLevelProvider provides daily price levels. Implemented by barbuf.DailyTracker.
type DailyLevelProvider interface {
	Levels(symbol string) DailyLevels
}

// DailyLevels holds precomputed daily price levels for a symbol.
type DailyLevels struct {
	PrevHigh  float64
	PrevLow   float64
	PrevClose float64
	TodayHigh float64
	TodayLow  float64
	TodayOpen float64
}

// RuntimeHelpersConsumer is an optional interface a strategy can implement to
// receive engine-provided helpers (bar buffer, daily levels) for script globals.
type RuntimeHelpersConsumer interface {
	SetRuntimeHelpers(bars BarBuffer, levels DailyLevelProvider)
}

// ContractSpecConsumer is an optional interface a strategy can implement to
// receive contract specifications (tick size, point value) for traded symbols.
// The engine calls SetContractSpecs after querying the provider during startup.
type ContractSpecConsumer interface {
	SetContractSpecs(specs map[string]ContractSpec)
}

// ContractSpec describes a tradeable instrument's tick and value properties.
// For equities and crypto: TickSize=0.01, PointValue=1.0.
// For futures: PointValue > 1 (e.g. MNQ=2.0, ES=50.0).
type ContractSpec struct {
	Symbol     string
	TickSize   float64 // minimum price increment
	TickValue  float64 // dollar value per tick
	PointValue float64 // dollar value per full point = tickValue / tickSize
}

// AssetQuery controls filtering for SearchAssets.
type AssetQuery struct {
	Text       string // free-text search (e.g. "Apple", "AAPL", "Bitcoin")
	AssetClass string // "us_equity", "crypto", "prediction", "forex", etc.
	Exchange   string // "XNAS", "XNYS", etc.
	Status     string // "active", "open" — provider normalizes
	Limit      int    // max results (0 = provider default)
}

// Asset represents a tradeable asset returned by AssetSearch.
type Asset struct {
	Symbol     string         // ticker symbol
	Name       string         // display name (company name, market title, etc.)
	AssetClass string         // "us_equity", "crypto", "prediction", etc.
	Exchange   string         // primary exchange
	Tradable   bool           // actively tradeable
	Extra      map[string]any // provider-specific fields
}

// AssetSearch is an optional interface a provider can implement to expose
// asset/symbol discovery and search capabilities. Strategies use this to
// dynamically find symbols to trade.
type AssetSearch interface {
	SearchAssets(ctx context.Context, query AssetQuery) ([]Asset, error)
}

// Deprecated: Use AssetSearch instead. Kept for backward compatibility with
// existing Kalshi scripts that call listMarkets().

// DiscoveredMarket represents a tradeable market returned by MarketDiscovery.
type DiscoveredMarket struct {
	Ticker       string
	Title        string
	Status       string // "open", "closed", "settled"
	EventTicker  string
	SeriesTicker string
	Volume       int
	Volume24H    int
	OpenTime     time.Time
	CloseTime    time.Time
}

// MarketListOptions controls filtering and pagination for ListMarkets.
type MarketListOptions struct {
	Status       string // "open", "closed", "settled", "" for all
	Limit        int    // max markets to return (0 = default 200, hard cap 1000)
	SeriesTicker string // filter by series (e.g. "KXMVE" for politics)
	EventTicker  string // filter by specific event
	MinVolume    int    // only return markets with volume >= this (client-side filter)
}

// MarketDiscovery is an optional interface a provider can implement to expose
// market listing and discovery capabilities.
// Deprecated: Implement AssetSearch instead.
type MarketDiscovery interface {
	ListMarkets(ctx context.Context, opts MarketListOptions) ([]DiscoveredMarket, error)
}
