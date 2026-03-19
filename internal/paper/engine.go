package paper

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata/stream"

	"brandon-bot/internal/db"
	"brandon-bot/internal/execution"
	"brandon-bot/internal/portfolio"
	"brandon-bot/internal/strategy"
)

// Config holds engine configuration.
type Config struct {
	Capital    float64
	Timeframe  string
	WarmupBars int // number of historical bars to replay on startup for indicator warm-up
}

func DefaultConfig(capital float64, timeframe string) Config {
	return Config{
		Capital:    capital,
		Timeframe:  timeframe,
		WarmupBars: 100,
	}
}

// engineEvent is a sum type that serializes all incoming events into a single
// channel so all strategy calls happen on one goroutine — strategies need not
// be thread-safe.
type engineEvent interface{ isEvent() }

type tickEvent struct{ tick strategy.Tick }
type tradeEvent struct{ trade strategy.Trade }
type fillEvent struct{ fill strategy.Fill }

func (tickEvent) isEvent()  {}
func (tradeEvent) isEvent() {}
func (fillEvent) isEvent()  {}

// Engine is the paper trading engine. It streams live data from Alpaca,
// calls the strategy on each event, submits returned orders, and processes
// async fill events — all serialized through a single event loop.
type Engine struct {
	strategy  strategy.Strategy
	portfolio *portfolio.SimulatedPortfolio
	executor  *execution.PaperExecutor
	store     *db.Store
	config    Config
	eventCh   chan engineEvent
}

func NewEngine(strat strategy.Strategy, exec *execution.PaperExecutor, store *db.Store, cfg Config) *Engine {
	return &Engine{
		strategy:  strat,
		portfolio: portfolio.NewSimulatedPortfolio(cfg.Capital),
		executor:  exec,
		store:     store,
		config:    cfg,
		eventCh:   make(chan engineEvent, 512),
	}
}

// Run starts the paper trading engine. It blocks until ctx is cancelled.
// feed should be "iex" (free) or "sip" (paid).
func (e *Engine) Run(ctx context.Context, symbols []string, feed string) error {
	// Seed portfolio and warm up strategy indicators from Alpaca state + recent history.
	if err := e.recover(symbols); err != nil {
		return fmt.Errorf("startup recovery: %w", err)
	}

	// Trade updates (fills) come in async — route through event channel so all
	// strategy calls remain on the processLoop goroutine.
	e.executor.Client.StreamTradeUpdatesInBackground(ctx, func(update alpaca.TradeUpdate) {
		if update.Event != "fill" && update.Event != "partial_fill" {
			return
		}
		if update.Price == nil || update.Qty == nil {
			return
		}
		f := strategy.Fill{
			Symbol:    update.Order.Symbol,
			Side:      string(update.Order.Side),
			Qty:       update.Qty.InexactFloat64(),
			Price:     update.Price.InexactFloat64(),
			Timestamp: update.At,
		}
		select {
		case e.eventCh <- fillEvent{fill: f}:
		case <-ctx.Done():
		}
	})

	sc := stream.NewStocksClient(
		marketdata.Feed(feed),
		stream.WithCredentials(os.Getenv("ALPACA_API_KEY"), os.Getenv("ALPACA_SECRET")),
	)

	// All strategies get bar events.
	if err := sc.SubscribeToBars(func(bar stream.Bar) {
		e.send(ctx, tickEvent{tick: strategy.Tick{
			Symbol:    bar.Symbol,
			Timestamp: bar.Timestamp,
			Open:      bar.Open,
			High:      bar.High,
			Low:       bar.Low,
			Close:     bar.Close,
			Volume:    int64(bar.Volume),
		}})
	}, symbols...); err != nil {
		return fmt.Errorf("subscribing to bars: %w", err)
	}

	// If the strategy implements TradeSubscriber, also subscribe to individual trades.
	if _, ok := e.strategy.(strategy.TradeSubscriber); ok {
		if err := sc.SubscribeToTrades(func(t stream.Trade) {
			e.send(ctx, tradeEvent{trade: strategy.Trade{
				Symbol:     t.Symbol,
				Timestamp:  t.Timestamp,
				Price:      t.Price,
				Size:       t.Size,
				Exchange:   t.Exchange,
				Conditions: t.Conditions,
			}})
		}, symbols...); err != nil {
			return fmt.Errorf("subscribing to trades: %w", err)
		}
		log.Printf("paper engine: trade-level subscription active for %v", symbols)
	}

	go e.processLoop(ctx)

	log.Printf("paper engine: connected, watching %v on feed=%s", symbols, feed)
	return sc.Connect(ctx) // blocks until ctx cancelled or fatal error
}

// send routes an event to the processing loop, dropping it with a warning if the
// channel is full (avoids blocking the WebSocket callback goroutine).
func (e *Engine) send(ctx context.Context, ev engineEvent) {
	select {
	case e.eventCh <- ev:
	case <-ctx.Done():
	default:
		log.Printf("warning: event channel full, dropping %T event", ev)
	}
}

func (e *Engine) processLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-e.eventCh:
			switch v := ev.(type) {
			case tickEvent:
				e.onTick(v.tick)
			case tradeEvent:
				e.onTrade(v.trade)
			case fillEvent:
				e.onFill(v.fill)
			}
		}
	}
}

func (e *Engine) onTick(tick strategy.Tick) {
	e.portfolio.UpdateMarketPrice(tick.Symbol, tick.Close)
	e.submitOrders(e.strategy.OnTick(tick, e.portfolio))
}

func (e *Engine) onTrade(trade strategy.Trade) {
	e.portfolio.UpdateMarketPrice(trade.Symbol, trade.Price)
	ts := e.strategy.(strategy.TradeSubscriber) // safe: only called when strategy implements it
	e.submitOrders(ts.OnTrade(trade, e.portfolio))
}

func (e *Engine) onFill(fill strategy.Fill) {
	e.portfolio.ApplyFill(fill)
	e.strategy.OnFill(fill)

	log.Printf("fill: %s %s qty=%.2f @ $%.2f", fill.Side, fill.Symbol, fill.Qty, fill.Price)

	if err := e.store.LogPaperFill(fill); err != nil {
		log.Printf("db: could not log fill: %v", err)
	}
	if err := e.store.LogPaperSnapshot(e.portfolio.Cash(), e.portfolio.Equity(), e.portfolio.TotalPL()); err != nil {
		log.Printf("db: could not log snapshot: %v", err)
	}
}

func (e *Engine) submitOrders(orders []strategy.Order) {
	for _, order := range orders {
		placed, err := e.executor.PlaceOrder(order)
		if err != nil {
			log.Printf("order error: %v", err)
			continue
		}
		log.Printf("order placed: %s %s qty=%.2f reason=%q alpacaID=%s",
			order.Side, order.Symbol, order.Qty, order.Reason, placed.ID)

		if err := e.store.LogPaperOrder(order, placed.ID); err != nil {
			log.Printf("db: could not log order: %v", err)
		}
	}
}
