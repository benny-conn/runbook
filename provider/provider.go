package provider

import (
	"context"
	"time"

	"github.com/benny-conn/brandon-bot/strategy"
)

// Bar is a completed OHLCV candlestick.
type Bar struct {
	Symbol    string
	Timestamp time.Time
	Open      float64
	High      float64
	Low       float64
	Close     float64
	Volume    float64
}

// Trade is a real-time trade print from the exchange.
type Trade struct {
	Symbol    string
	Timestamp time.Time
	Price     float64
	Size      float64
}

// Quote is a real-time bid/ask update from the exchange.
type Quote struct {
	Symbol    string
	Timestamp time.Time
	BidPrice  float64
	BidSize   float64
	AskPrice  float64
	AskSize   float64
}

// OpenOrder is a pending order that has not yet been fully filled or cancelled.
type OpenOrder struct {
	ID         string
	Symbol     string
	Side       string  // "buy" or "sell"
	Qty        float64 // total ordered quantity
	Filled     float64 // quantity filled so far
	OrderType  string  // "market", "limit", "stop", "stop_limit"
	LimitPrice float64
	StopPrice  float64
}

// Fill is a completed or partial order execution.
type Fill struct {
	OrderID   string
	Symbol    string
	Side      string
	Qty       float64
	Price     float64
	Timestamp time.Time
	Partial   bool
}

// OrderResult is returned by PlaceOrder.
type OrderResult struct {
	ID string
}

// Account holds top-level account state.
type Account struct {
	Cash   float64
	Equity float64
}

// Position is an open holding.
type Position struct {
	Symbol        string
	Qty           float64
	AvgEntryPrice float64
	CurrentPrice  float64
}

// MarketData provides historical and real-time price data.
// Timeframe strings use canonical short form: "1s", "1m", "5m", "15m", "1h", "1d".
type MarketData interface {
	// FetchBars returns historical OHLCV bars for a single symbol.
	FetchBars(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]Bar, error)
	// FetchBarsMulti returns bars for multiple symbols, merged and sorted by timestamp.
	FetchBarsMulti(ctx context.Context, symbols []string, timeframe string, start, end time.Time) ([]Bar, error)
	// SubscribeBars streams live completed bars, calling handler for each.
	// Blocks until ctx is cancelled.
	SubscribeBars(ctx context.Context, symbols []string, timeframe string, handler func(Bar)) error
	// SubscribeTrades streams live individual trade prints, calling handler for each.
	// Blocks until ctx is cancelled.
	SubscribeTrades(ctx context.Context, symbols []string, handler func(Trade)) error
	// SubscribeQuotes streams live bid/ask quote updates, calling handler for each.
	// Blocks until ctx is cancelled.
	SubscribeQuotes(ctx context.Context, symbols []string, handler func(Quote)) error
}

// Execution manages orders and account state.
type Execution interface {
	// GetAccount returns current cash and equity.
	GetAccount(ctx context.Context) (Account, error)
	// GetPositions returns all currently open positions.
	GetPositions(ctx context.Context) ([]Position, error)
	// GetOpenOrders returns all orders that are pending or partially filled.
	GetOpenOrders(ctx context.Context) ([]OpenOrder, error)
	// PlaceOrder submits an order and returns the broker-assigned ID.
	PlaceOrder(ctx context.Context, order strategy.Order) (OrderResult, error)
	// CancelOrder cancels a pending order by broker-assigned ID.
	CancelOrder(ctx context.Context, orderID string) error
	// SubscribeFills streams fill and partial-fill events.
	// Blocks until ctx is cancelled.
	SubscribeFills(ctx context.Context, handler func(Fill)) error
}

// SessionEvent describes a market session transition.
type SessionEvent struct {
	Type string // "market_open" or "market_close"
	Time time.Time
}

// SessionNotifier is an optional interface a provider can implement to emit
// market-open and market-close events. If the provider supports it, the engine
// subscribes so it can call the strategy's DailySessionHandler hooks.
type SessionNotifier interface {
	SubscribeSession(ctx context.Context, handler func(SessionEvent)) error
}

// ContinuousMarket is an optional marker interface a provider can implement
// to indicate it trades 24/7 with no discrete market sessions (e.g. prediction
// markets, crypto). When implemented, the engine skips DailySessionHandler
// hooks entirely — even if the strategy implements them.
type ContinuousMarket interface {
	ContinuousMarket()
}

// ClientSideStops is an optional marker interface a provider can implement
// to indicate it doesn't support native stop or stop-limit orders. When
// implemented, the engine intercepts stop/stop_limit orders and manages
// them locally, triggering them based on incoming price data.
type ClientSideStops interface {
	ClientSideStops()
}

// ContractSpec describes a futures contract's tick/point value properties.
type ContractSpec struct {
	Symbol     string
	TickSize   float64 // minimum price increment (e.g. 0.25 for MNQ)
	TickValue  float64 // dollar value per tick (e.g. 0.50 for MNQ)
	PointValue float64 // dollar value per full point = tickValue / tickSize (e.g. 2.0 for MNQ)
}

// ContractSpecProvider is an optional interface a provider can implement to
// expose futures contract specifications. The engine uses PointValue as the
// P&L multiplier — for equities the default is 1.0.
type ContractSpecProvider interface {
	GetContractSpec(ctx context.Context, symbol string) (ContractSpec, error)
}

// BarToTick converts a provider Bar to a strategy Tick.
func BarToTick(b Bar) strategy.Tick {
	return strategy.Tick{
		Symbol:    b.Symbol,
		Timestamp: b.Timestamp,
		Open:      b.Open,
		High:      b.High,
		Low:       b.Low,
		Close:     b.Close,
		Volume:    int64(b.Volume),
	}
}
