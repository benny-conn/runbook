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
type Order struct {
	Symbol     string
	Side       string  // "buy" or "sell"
	Qty        float64
	OrderType  string  // "market", "limit", "stop", or "stop_limit"
	LimitPrice float64 // limit price for limit and stop-limit orders
	StopPrice  float64 // trigger price for stop and stop-limit orders
	StopLoss   float64 // broker-native bracket stop loss price (0 = disabled)
	TakeProfit float64 // broker-native bracket take profit price (0 = disabled)
	Reason     string  // for logging/debugging
}

// Fill is the result of an executed order.
type Fill struct {
	Symbol    string
	Side      string
	Qty       float64
	Price     float64
	Timestamp time.Time
}

// Position represents an open holding in the portfolio.
type Position struct {
	Symbol       string
	Qty          float64
	AvgCost      float64
	MarketValue  float64
	UnrealizedPL float64
}

// Portfolio is a read-only view of the current account state passed into OnTick.
type Portfolio interface {
	Cash() float64
	Equity() float64
	Position(symbol string) *Position
	Positions() []Position
	TotalPL() float64
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

// Configurable is an optional interface a strategy can implement to accept
// a JSON config file passed via --config on the CLI. Configure is called once
// after the strategy is constructed and before the first OnTick, so it can
// override any defaults set in the constructor.
// Partial configs are fine — only fields present in the JSON are updated;
// missing fields keep their constructor defaults.
type Configurable interface {
	Configure(data []byte) error
}

// Strategy is implemented by any trading algorithm.
// The engine calls OnTick on every price update and executes any returned orders.
// All strategy state must live inside the Strategy implementation — the engine is stateless w.r.t. strategy internals.
type Strategy interface {
	Name() string
	OnTick(tick Tick, portfolio Portfolio) []Order
	OnFill(fill Fill)
}

// TradeSubscriber is an optional interface a strategy can implement to receive
// individual trade prints instead of (or in addition to) completed bars.
// If the strategy implements this, the paper engine will also subscribe to
// the trade stream for the requested symbols.
// OnTick is still called for bar events — implement it as a no-op if not needed.
type TradeSubscriber interface {
	OnTrade(trade Trade, portfolio Portfolio) []Order
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
	OnQuote(quote Quote, portfolio Portfolio) []Order
}

// InitContext holds the context passed to a strategy's OnInit hook and
// SymbolResolver.ResolveSymbols.
type InitContext struct {
	Symbols   []string
	Timeframe string
	Config    []byte // raw JSON config (nil if none)

	// Discovery provides market listing capabilities if the provider supports it
	// (e.g. Kalshi's market catalog). Nil if the provider doesn't implement MarketDiscovery.
	Discovery MarketDiscovery

	// AddSymbols dynamically subscribes to new symbols mid-run. Call from OnTick,
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
	OnMarketOpen(portfolio Portfolio) []Order
	OnMarketClose(portfolio Portfolio) []Order
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
// market listing and discovery capabilities. Prediction market providers like
// Kalshi implement this so strategies can find active markets to trade.
type MarketDiscovery interface {
	ListMarkets(ctx context.Context, opts MarketListOptions) ([]DiscoveredMarket, error)
}
