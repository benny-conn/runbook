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

// engineEvent is a sum type that serializes ticks and fills into a single channel,
// so all strategy calls happen on one goroutine and strategies need not be thread-safe.
type engineEvent interface{ isEvent() }

type tickEvent struct{ tick strategy.Tick }
type fillEvent struct{ fill strategy.Fill }

func (tickEvent) isEvent() {}
func (fillEvent) isEvent() {}

// Engine is the paper trading engine. It streams live bars from Alpaca,
// calls the strategy on each tick, submits returned orders, and processes
// async fill events — all serialized through a single event loop.
type Engine struct {
	strategy  strategy.Strategy
	portfolio *portfolio.SimulatedPortfolio
	executor  *execution.PaperExecutor
	store     *db.Store
	eventCh   chan engineEvent
}

func NewEngine(strat strategy.Strategy, exec *execution.PaperExecutor, store *db.Store, capital float64) *Engine {
	return &Engine{
		strategy:  strat,
		portfolio: portfolio.NewSimulatedPortfolio(capital),
		executor:  exec,
		store:     store,
		eventCh:   make(chan engineEvent, 512),
	}
}

// Run starts the paper trading engine. It blocks until ctx is cancelled.
// feed should be "iex" (free) or "sip" (paid).
func (e *Engine) Run(ctx context.Context, symbols []string, feed string) error {
	// Trade updates come in asynchronously from Alpaca — route them through the event channel
	// so all strategy calls remain on the processLoop goroutine.
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

	if err := sc.SubscribeToBars(func(bar stream.Bar) {
		tick := strategy.Tick{
			Symbol:    bar.Symbol,
			Timestamp: bar.Timestamp,
			Open:      bar.Open,
			High:      bar.High,
			Low:       bar.Low,
			Close:     bar.Close,
			Volume:    int64(bar.Volume),
		}
		select {
		case e.eventCh <- tickEvent{tick: tick}:
		case <-ctx.Done():
		default:
			log.Printf("warning: event channel full, dropping bar for %s", bar.Symbol)
		}
	}, symbols...); err != nil {
		return fmt.Errorf("subscribing to bars: %w", err)
	}

	go e.processLoop(ctx)

	log.Printf("paper engine: connected, watching %v on feed=%s", symbols, feed)
	return sc.Connect(ctx) // blocks until ctx cancelled or fatal error
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
			case fillEvent:
				e.onFill(v.fill)
			}
		}
	}
}

func (e *Engine) onTick(tick strategy.Tick) {
	e.portfolio.UpdateMarketPrice(tick.Symbol, tick.Close)

	orders := e.strategy.OnTick(tick, e.portfolio)
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

func (e *Engine) onFill(fill strategy.Fill) {
	e.portfolio.ApplyFill(fill)
	e.strategy.OnFill(fill)

	log.Printf("fill: %s %s qty=%.2f @ $%.2f", fill.Side, fill.Symbol, fill.Qty, fill.Price)

	if err := e.store.LogPaperFill(fill); err != nil {
		log.Printf("db: could not log fill: %v", err)
	}

	snap := e.portfolio
	if err := e.store.LogPaperSnapshot(snap.Cash(), snap.Equity(), snap.TotalPL()); err != nil {
		log.Printf("db: could not log snapshot: %v", err)
	}
}
