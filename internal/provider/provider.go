package provider

import (
	"context"
	"time"

	"brandon-bot/internal/strategy"
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
}

// Execution manages orders and account state.
type Execution interface {
	// GetAccount returns current cash and equity.
	GetAccount(ctx context.Context) (Account, error)
	// GetPositions returns all currently open positions.
	GetPositions(ctx context.Context) ([]Position, error)
	// PlaceOrder submits an order and returns the broker-assigned ID.
	PlaceOrder(ctx context.Context, order strategy.Order) (OrderResult, error)
	// SubscribeFills streams fill and partial-fill events.
	// Blocks until ctx is cancelled.
	SubscribeFills(ctx context.Context, handler func(Fill)) error
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
