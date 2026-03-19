package strategy

import "time"

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
	OrderType  string  // "market" or "limit"
	LimitPrice float64
	Reason     string // for logging/debugging
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
