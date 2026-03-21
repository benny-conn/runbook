package engine

import (
	"context"
	"fmt"
	"log"

	"brandon-bot/internal/db"
	"brandon-bot/internal/portfolio"
	"brandon-bot/provider"
	"brandon-bot/strategy"
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

// Engine is the paper trading engine. It streams live data from the provider,
// calls the strategy on each event, submits returned orders, and processes
// async fill events — all serialized through a single event loop.
type Engine struct {
	strategy  strategy.Strategy
	portfolio *portfolio.SimulatedPortfolio
	md        provider.MarketData
	exec      provider.Execution
	store     *db.Store
	config    Config
	eventCh   chan engineEvent
	ctx       context.Context // set in Run, used by submitOrders
}

// NewEngine constructs a paper trading engine. md and exec may be the same
// object (e.g. *alpaca.Provider) or separate implementations.
func NewEngine(strat strategy.Strategy, md provider.MarketData, exec provider.Execution, store *db.Store, cfg Config) *Engine {
	return &Engine{
		strategy:  strat,
		portfolio: portfolio.NewSimulatedPortfolio(cfg.Capital),
		md:        md,
		exec:      exec,
		store:     store,
		config:    cfg,
		eventCh:   make(chan engineEvent, 512),
	}
}

// Run starts the paper trading engine. It blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context, symbols []string) error {
	e.ctx = ctx

	// Seed portfolio and warm up strategy indicators from account state + recent history.
	if err := e.recover(ctx, symbols); err != nil {
		return fmt.Errorf("startup recovery: %w", err)
	}

	// Fill events come in async — route through event channel so all strategy
	// calls remain on the processLoop goroutine.
	go func() {
		if err := e.exec.SubscribeFills(ctx, func(f provider.Fill) {
			e.send(ctx, fillEvent{fill: strategy.Fill{
				Symbol:    f.Symbol,
				Side:      f.Side,
				Qty:       f.Qty,
				Price:     f.Price,
				Timestamp: f.Timestamp,
			}})
		}); err != nil && ctx.Err() == nil {
			log.Printf("paper engine: fill subscription error: %v", err)
		}
	}()

	// If the strategy implements TradeSubscriber, subscribe to individual trades.
	if _, ok := e.strategy.(strategy.TradeSubscriber); ok {
		go func() {
			if err := e.md.SubscribeTrades(ctx, symbols, func(t provider.Trade) {
				e.send(ctx, tradeEvent{trade: strategy.Trade{
					Symbol:    t.Symbol,
					Timestamp: t.Timestamp,
					Price:     t.Price,
					Size:      uint32(t.Size),
				}})
			}); err != nil && ctx.Err() == nil {
				log.Printf("paper engine: trade subscription error: %v", err)
			}
		}()
		log.Printf("paper engine: trade-level subscription active for %v", symbols)
	}

	go e.processLoop(ctx)

	log.Printf("paper engine: connecting to bar stream | symbols=%v timeframe=%s", symbols, e.config.Timeframe)

	// SubscribeBars blocks until ctx is cancelled — this is the main run loop.
	return e.md.SubscribeBars(ctx, symbols, e.config.Timeframe, func(b provider.Bar) {
		e.send(ctx, tickEvent{tick: provider.BarToTick(b)})
	})
}

// send routes an event to the processing loop, dropping it with a warning if
// the channel is full (avoids blocking the WebSocket callback goroutine).
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
		result, err := e.exec.PlaceOrder(e.ctx, order)
		if err != nil {
			log.Printf("order error: %v", err)
			continue
		}
		log.Printf("order placed: %s %s qty=%.2f reason=%q id=%s",
			order.Side, order.Symbol, order.Qty, order.Reason, result.ID)

		if err := e.store.LogPaperOrder(order, result.ID); err != nil {
			log.Printf("db: could not log order: %v", err)
		}
	}
}
