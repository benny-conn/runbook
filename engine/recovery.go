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

// PositionSeeder is an optional interface a strategy can implement to accept
// position state injected during warm-up recovery. If a strategy doesn't
// implement this, position reconciliation is skipped (indicator warm-up still runs).
type PositionSeeder interface {
	SeedPosition(symbol string, qty, avgCost float64)
}

// recover runs on startup before the live stream begins:
//  1. Queries the broker for real account cash + open positions → seeds portfolio
//  2. Fetches recent historical bars → replays through strategy (no orders placed)
//     so EMAs and any other rolling indicators are properly warmed up
//  3. If the strategy implements PositionSeeder, injects known positions so it
//     knows whether it's currently holding something and at what cost
func (e *Engine) recover(ctx context.Context, symbols []string) error {
	log.Println("recovery: fetching account state...")

	// Determine cash: use override if set, otherwise query broker.
	var cash float64
	if e.config.CashOverride > 0 {
		cash = e.config.CashOverride
		log.Printf("recovery: using cash override $%.2f", cash)
	} else {
		account, err := e.exec.GetAccount(ctx)
		if err != nil {
			return fmt.Errorf("getting account: %w", err)
		}
		cash = account.Cash
	}

	// Determine positions: use override if set, otherwise query broker.
	// A non-nil Positions slice (even if empty) means "use this instead of broker".
	var positions []provider.Position
	if e.config.Positions != nil {
		positions = e.config.Positions
		log.Printf("recovery: using %d backend-supplied positions (bypassing broker)", len(positions))
	} else {
		var err error
		positions, err = e.exec.GetPositions(ctx)
		if err != nil {
			return fmt.Errorf("getting positions: %w", err)
		}
	}

	// Seed portfolio with cash balance.
	e.portfolio = portfolio.NewSimulatedPortfolio(cash)

	// Apply any existing open positions.
	seedPositions(e.portfolio, positions)

	log.Printf("recovery: portfolio seeded — cash=$%.2f equity=$%.2f open_positions=%d",
		e.portfolio.Cash(), e.portfolio.Equity(), len(positions))

	// Fetch enough recent bars to warm up the strategy's rolling indicators.
	// If WarmupFrom is set (e.g. strategy creation date), warm up from that time
	// but cap at MaxWarmupBars to avoid fetching years of data.
	end := time.Now()
	start := end.Add(-warmupWindow(e.baseTimeframe, e.config.WarmupBars))

	if !e.config.WarmupFrom.IsZero() {
		fromStart := e.config.WarmupFrom
		maxBars := e.config.MaxWarmupBars
		if maxBars <= 0 {
			maxBars = 300
		}
		maxStart := end.Add(-warmupWindow(e.baseTimeframe, maxBars))
		if fromStart.Before(maxStart) {
			fromStart = maxStart
		}
		if fromStart.Before(start) {
			start = fromStart
		}
		log.Printf("recovery: using WarmupFrom=%s (capped at %d bars)", e.config.WarmupFrom.Format("2006-01-02"), maxBars)
	}

	log.Printf("recovery: fetching %s history from %s for warm-up...", e.baseTimeframe, start.Format("2006-01-02"))

	bars, err := e.md.FetchBarsMulti(ctx, symbols, e.baseTimeframe, start, end)
	if err != nil {
		log.Printf("recovery: warm-up bar fetch failed (non-fatal, skipping replay): %v", err)
	} else {
		e.warmingUp = true
		log.Printf("recovery: replaying %d bars through strategy (simulating fills locally)...", len(bars))

		// Check if the strategy supports daily session hooks.
		dsh, hasDailyHooks := e.strategy.(strategy.DailySessionHandler)
		var currentDate string

		// Use market-timezone-aware dates for session boundaries instead of UTC.
		// This ensures futures (e.g. ES, MNQ) get correct day boundaries aligned
		// with market hours rather than UTC midnight.
		dateLoc := marketDateLocation(e.config.MarketSchedule)
		dateOf := func(t time.Time) string {
			return t.In(dateLoc).Format("2006-01-02")
		}

		for _, b := range bars {
			tick := provider.BarToTick(b)
			date := dateOf(tick.Timestamp)

			// Fire daily lifecycle hooks at day boundaries.
			if hasDailyHooks && date != currentDate {
				if currentDate != "" {
					// End of previous day — fire OnMarketClose.
					closeOrders := dsh.OnMarketClose(e.portfolio)
					simulateFills(e.strategy, e.portfolio, closeOrders, tick)
				}
				currentDate = date
				// Start of new day — fire OnMarketOpen.
				e.portfolio.UpdateMarketPrice(tick.Symbol, tick.Close)
				openOrders := dsh.OnMarketOpen(e.portfolio)
				simulateFills(e.strategy, e.portfolio, openOrders, tick)
			} else {
				e.portfolio.UpdateMarketPrice(tick.Symbol, tick.Close)
			}

			// handleBar calls OnBar + feeds aggregators; warmingUp flag
			// ensures any returned orders are simulated locally.
			e.handleBar(e.baseTimeframe, tick)
		}

		// Fire final OnMarketClose so strategy state is up to date.
		if hasDailyHooks && currentDate != "" {
			closeOrders := dsh.OnMarketClose(e.portfolio)
			if len(bars) > 0 {
				lastTick := provider.BarToTick(bars[len(bars)-1])
				simulateFills(e.strategy, e.portfolio, closeOrders, lastTick)
			}
		}
	}

	e.warmingUp = false

	// Reset portfolio to the real broker/backend state after warmup replay.
	// Simulated fills shifted balances/positions — restore truth before going live.
	e.portfolio = portfolio.NewSimulatedPortfolio(cash)
	seedPositions(e.portfolio, positions)

	// If the strategy supports position injection, tell it what we currently hold.
	// Pass raw qty (negative for shorts) so the strategy can track direction.
	if seeder, ok := e.strategy.(PositionSeeder); ok {
		for _, pos := range positions {
			seeder.SeedPosition(pos.Symbol, pos.Qty, pos.AvgEntryPrice)
			log.Printf("recovery: injected position into strategy — %s qty=%.2f avgCost=%.2f",
				pos.Symbol, pos.Qty, pos.AvgEntryPrice)
		}
	}

	log.Println("recovery: complete — ready to trade")
	return nil
}

// seedPositions applies broker positions to the simulated portfolio.
// Handles both long (positive qty) and short (negative qty) positions.
func seedPositions(port *portfolio.SimulatedPortfolio, positions []provider.Position) {
	for _, pos := range positions {
		side := "buy"
		qty := pos.Qty
		if qty < 0 {
			side = "sell"
			qty = -qty
		}
		port.ApplyFill(strategy.Fill{
			Symbol: pos.Symbol,
			Side:   side,
			Qty:    qty,
			Price:  pos.AvgEntryPrice,
		})
		if pos.CurrentPrice > 0 {
			port.UpdateMarketPrice(pos.Symbol, pos.CurrentPrice)
		}
	}
}

// simulateFills locally fills market orders during warmup replay so that
// stateful strategies (like the script ORB) transition correctly through
// their lifecycle. Non-market orders are ignored — limit/stop fills can't
// be reliably simulated from bar data alone.
//
// LIMITATION: Strategies that rely on limit orders for entries will not have
// those fills replicated during warmup. This means indicator state and position
// tracking may be slightly incorrect after recovery. For best results, use
// market orders for entries or implement PositionSeeder to reconcile state.
func simulateFills(strat strategy.Strategy, port *portfolio.SimulatedPortfolio, orders []strategy.Order, tick strategy.Tick) {
	for _, o := range orders {
		if o.OrderType != "market" && o.OrderType != "" {
			continue
		}
		fill := strategy.Fill{
			Symbol:    o.Symbol,
			Side:      o.Side,
			Qty:       o.Qty,
			Price:     tick.Close,
			Timestamp: tick.Timestamp,
		}
		port.ApplyFill(fill)
		strat.OnFill(fill)
	}
}

// marketDateLocation returns the timezone location for determining trading day
// boundaries. Uses the configured market schedule timezone, falling back to
// America/New_York (NYSE default) and then UTC.
func marketDateLocation(sched *MarketSchedule) *time.Location {
	if sched != nil && sched.Timezone != "" {
		if loc, err := time.LoadLocation(sched.Timezone); err == nil {
			return loc
		}
	}
	// Default to NYSE timezone.
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return loc
	}
	return time.UTC
}

// warmupWindow returns how far back to fetch historical bars.
// For daily bars we go back bars*2 calendar days.
// For sub-day bars we fetch 10 calendar days — always enough bars at any frequency.
func warmupWindow(timeframe string, bars int) time.Duration {
	if timeframe == "1d" {
		return time.Duration(bars*2) * 24 * time.Hour
	}
	return 10 * 24 * time.Hour
}
