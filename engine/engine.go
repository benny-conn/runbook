package engine

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/benny-conn/brandon-bot/internal/portfolio"
	"github.com/benny-conn/brandon-bot/provider"
	"github.com/benny-conn/brandon-bot/strategy"
)

// Store is the persistence interface used by the engine to log orders, fills,
// and portfolio snapshots. The engine doesn't care whether the underlying
// implementation targets SQLite, Postgres, or anything else — callers provide
// their own implementation.
type Store interface {
	LogOrder(order strategy.Order, brokerOrderID string) error
	LogFill(fill strategy.Fill) error
	LogSnapshot(cash, equity, totalPL float64) error
}

// MarketSchedule defines when the market opens and closes for clock-based
// session events. Used as a fallback when the provider doesn't implement
// SessionNotifier.
type MarketSchedule struct {
	Open     string // "09:30" (24h format, HH:MM)
	Close    string // "16:00"
	Timezone string // IANA timezone, e.g. "America/New_York"
}

// NYSESchedule returns the default NYSE/NASDAQ market schedule.
func NYSESchedule() *MarketSchedule {
	return &MarketSchedule{
		Open:     "09:30",
		Close:    "16:00",
		Timezone: "America/New_York",
	}
}

// Config holds engine configuration.
type Config struct {
	Capital        float64
	Timeframe      string
	WarmupBars     int             // number of historical bars to replay on startup for indicator warm-up
	ConfigJSON     []byte          // raw JSON config passed to Initializer.OnInit (nil if none)
	MarketSchedule *MarketSchedule // market hours for DailySessionHandler; nil defaults to NYSE
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
type quoteEvent struct{ quote strategy.Quote }
type marketOpenEvent struct{}
type marketCloseEvent struct{}

func (tickEvent) isEvent()        {}
func (tradeEvent) isEvent()       {}
func (fillEvent) isEvent()        {}
func (quoteEvent) isEvent()       {}
func (marketOpenEvent) isEvent()  {}
func (marketCloseEvent) isEvent() {}

// Engine is the paper trading engine. It streams live data from the provider,
// calls the strategy on each event, submits returned orders, and processes
// async fill events — all serialized through a single event loop.
type Engine struct {
	strategy  strategy.Strategy
	portfolio *portfolio.SimulatedPortfolio
	md        provider.MarketData
	exec      provider.Execution
	store     Store
	config    Config
	eventCh   chan engineEvent
	ctx       context.Context // set in Run, used by submitOrders
}

// NewEngine constructs a paper trading engine. md and exec may be the same
// object (e.g. *alpaca.Provider) or separate implementations.
func NewEngine(strat strategy.Strategy, md provider.MarketData, exec provider.Execution, store Store, cfg Config) *Engine {
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

	// If the strategy implements Initializer, call OnInit before any market data.
	if init, ok := e.strategy.(strategy.Initializer); ok {
		log.Println("paper engine: calling OnInit...")
		if err := init.OnInit(strategy.InitContext{
			Symbols:   symbols,
			Timeframe: e.config.Timeframe,
			Config:    e.config.ConfigJSON,
		}); err != nil {
			return fmt.Errorf("strategy OnInit: %w", err)
		}
		log.Println("paper engine: OnInit complete")
	}

	// If the strategy implements Shutdowner, call OnExit when the engine stops.
	if sd, ok := e.strategy.(strategy.Shutdowner); ok {
		defer func() {
			log.Println("paper engine: calling OnExit...")
			sd.OnExit()
			log.Println("paper engine: OnExit complete")
		}()
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

	// If the strategy implements QuoteSubscriber, subscribe to bid/ask quotes.
	if _, ok := e.strategy.(strategy.QuoteSubscriber); ok {
		go func() {
			if err := e.md.SubscribeQuotes(ctx, symbols, func(q provider.Quote) {
				e.send(ctx, quoteEvent{quote: strategy.Quote{
					Symbol:    q.Symbol,
					Timestamp: q.Timestamp,
					BidPrice:  q.BidPrice,
					BidSize:   q.BidSize,
					AskPrice:  q.AskPrice,
					AskSize:   q.AskSize,
				}})
			}); err != nil && ctx.Err() == nil {
				log.Printf("paper engine: quote subscription error: %v", err)
			}
		}()
		log.Printf("paper engine: quote-level subscription active for %v", symbols)
	}

	// If the strategy implements DailySessionHandler, subscribe to market
	// open/close events. Prefer provider-driven events if available, otherwise
	// fall back to clock-based scheduling using MarketSchedule config.
	if _, ok := e.strategy.(strategy.DailySessionHandler); ok {
		if sn, ok := e.md.(provider.SessionNotifier); ok {
			go func() {
				if err := sn.SubscribeSession(ctx, func(ev provider.SessionEvent) {
					switch ev.Type {
					case "market_open":
						e.send(ctx, marketOpenEvent{})
					case "market_close":
						e.send(ctx, marketCloseEvent{})
					}
				}); err != nil && ctx.Err() == nil {
					log.Printf("paper engine: session subscription error: %v", err)
				}
			}()
			log.Println("paper engine: market session subscription active (provider-driven)")
		} else {
			sched := e.config.MarketSchedule
			if sched == nil {
				sched = NYSESchedule()
			}
			go e.runSessionClock(ctx, sched)
			log.Printf("paper engine: market session subscription active (clock-based: open=%s close=%s tz=%s)",
				sched.Open, sched.Close, sched.Timezone)
		}
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
			case quoteEvent:
				e.onQuote(v.quote)
			case marketOpenEvent:
				e.onMarketOpen()
			case marketCloseEvent:
				e.onMarketClose()
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

func (e *Engine) onQuote(quote strategy.Quote) {
	qs := e.strategy.(strategy.QuoteSubscriber) // safe: only called when strategy implements it
	e.submitOrders(qs.OnQuote(quote, e.portfolio))
}

func (e *Engine) onMarketOpen() {
	dsh := e.strategy.(strategy.DailySessionHandler) // safe: only called when strategy implements it
	log.Println("paper engine: market open — calling OnMarketOpen")
	e.submitOrders(dsh.OnMarketOpen(e.portfolio))
}

func (e *Engine) onMarketClose() {
	dsh := e.strategy.(strategy.DailySessionHandler) // safe: only called when strategy implements it
	log.Println("paper engine: market close — calling OnMarketClose")
	e.submitOrders(dsh.OnMarketClose(e.portfolio))
}

func (e *Engine) onFill(fill strategy.Fill) {
	e.portfolio.ApplyFill(fill)
	e.strategy.OnFill(fill)

	log.Printf("fill: %s %s qty=%.2f @ $%.2f", fill.Side, fill.Symbol, fill.Qty, fill.Price)

	if err := e.store.LogFill(fill); err != nil {
		log.Printf("db: could not log fill: %v", err)
	}
	if err := e.store.LogSnapshot(e.portfolio.Cash(), e.portfolio.Equity(), e.portfolio.TotalPL()); err != nil {
		log.Printf("db: could not log snapshot: %v", err)
	}
}

// runSessionClock fires marketOpenEvent and marketCloseEvent based on wall-clock
// time. It skips weekends (Saturday/Sunday) and sleeps until the next event.
func (e *Engine) runSessionClock(ctx context.Context, sched *MarketSchedule) {
	loc, err := time.LoadLocation(sched.Timezone)
	if err != nil {
		log.Printf("paper engine: invalid market timezone %q: %v — session clock disabled", sched.Timezone, err)
		return
	}

	openH, openM := parseHHMM(sched.Open)
	closeH, closeM := parseHHMM(sched.Close)

	for {
		now := time.Now().In(loc)
		openTime, closeTime := nextSessionTimes(now, openH, openM, closeH, closeM, loc)

		// Decide which event comes next.
		var nextTime time.Time
		var ev engineEvent
		if now.Before(openTime) {
			nextTime = openTime
			ev = marketOpenEvent{}
		} else if now.Before(closeTime) {
			nextTime = closeTime
			ev = marketCloseEvent{}
		} else {
			// Past today's close — advance to next weekday's open.
			tomorrow := now.AddDate(0, 0, 1)
			openTime, _ = nextSessionTimes(tomorrow, openH, openM, closeH, closeM, loc)
			nextTime = openTime
			ev = marketOpenEvent{}
		}

		delay := time.Until(nextTime)
		log.Printf("paper engine: next session event (%T) in %s at %s",
			ev, delay.Round(time.Second), nextTime.Format("2006-01-02 15:04:05 MST"))

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
			e.send(ctx, ev)
		}
	}
}

// nextSessionTimes returns today's (or the next weekday's) open and close
// times, skipping Saturday and Sunday.
func nextSessionTimes(now time.Time, openH, openM, closeH, closeM int, loc *time.Location) (open, close time.Time) {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	// Skip to Monday if on a weekend.
	for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
		day = day.AddDate(0, 0, 1)
	}

	open = time.Date(day.Year(), day.Month(), day.Day(), openH, openM, 0, 0, loc)
	close = time.Date(day.Year(), day.Month(), day.Day(), closeH, closeM, 0, 0, loc)
	return
}

// parseHHMM parses a "HH:MM" string into hour and minute ints.
func parseHHMM(s string) (int, int) {
	var h, m int
	fmt.Sscanf(s, "%d:%d", &h, &m)
	return h, m
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

		if err := e.store.LogOrder(order, result.ID); err != nil {
			log.Printf("db: could not log order: %v", err)
		}
	}
}
