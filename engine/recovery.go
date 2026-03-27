package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/benny-conn/brandon-bot/internal/bracket"
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
	e.logf("recovery: fetching account state...")

	// Determine cash: use override if set, otherwise query broker.
	var cash float64
	if e.config.CashOverride > 0 {
		cash = e.config.CashOverride
		e.logf("recovery: using cash override $%.2f", cash)
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
		e.logf("recovery: using %d backend-supplied positions (bypassing broker)", len(positions))
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

	e.logf("recovery: portfolio seeded — cash=$%.2f equity=$%.2f open_positions=%d",
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
		e.logf("recovery: using WarmupFrom=%s (capped at %d bars)", e.config.WarmupFrom.Format("2006-01-02"), maxBars)
	}

	e.logf("recovery: fetching %s history from %s for warm-up...", e.baseTimeframe, start.Format("2006-01-02"))

	bars, err := e.md.FetchBarsMulti(ctx, symbols, e.baseTimeframe, start, end)
	if err != nil {
		e.logf("recovery: warm-up bar fetch failed (non-fatal, skipping replay): %v", err)
	} else {
		e.warmingUp = true
		e.logf("recovery: replaying %d bars through strategy (simulating fills locally)...", len(bars))

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
					e.strategy.SetPortfolio(e.portfolio)
					closeOrders := dsh.OnMarketClose()
					simulateFills(e.strategy, e.portfolio, closeOrders, tick, e)
				}
				currentDate = date
				// Start of new day — fire OnMarketOpen.
				e.portfolio.UpdateMarketPrice(tick.Symbol, tick.Close)
				e.portfolio.ResetDaily()
				e.strategy.SetPortfolio(e.portfolio)
				openOrders := dsh.OnMarketOpen()
				simulateFills(e.strategy, e.portfolio, openOrders, tick, e)
			} else {
				e.portfolio.UpdateMarketPrice(tick.Symbol, tick.Close)
			}

			// handleBar calls OnBar + feeds aggregators; warmingUp flag
			// ensures any returned orders are simulated locally.
			e.handleBar(e.baseTimeframe, tick)
		}

		// Fire final OnMarketClose so strategy state is up to date.
		if hasDailyHooks && currentDate != "" {
			e.strategy.SetPortfolio(e.portfolio)
			closeOrders := dsh.OnMarketClose()
			if len(bars) > 0 {
				lastTick := provider.BarToTick(bars[len(bars)-1])
				simulateFills(e.strategy, e.portfolio, closeOrders, lastTick, e)
			}
		}
	}

	e.warmingUp = false
	e.warmupBrackets = nil // clear simulated brackets

	// Reset portfolio to the real broker/backend state after warmup replay.
	// Simulated fills shifted balances/positions — restore truth before going live.
	e.portfolio = portfolio.NewSimulatedPortfolio(cash)
	seedPositions(e.portfolio, positions)

	// If the strategy supports position injection, tell it what we currently hold.
	// Pass raw qty (negative for shorts) so the strategy can track direction.
	if seeder, ok := e.strategy.(PositionSeeder); ok {
		for _, pos := range positions {
			seeder.SeedPosition(pos.Symbol, pos.Qty, pos.AvgEntryPrice)
			e.logf("recovery: injected position into strategy — %s qty=%.2f avgCost=%.2f",
				pos.Symbol, pos.Qty, pos.AvgEntryPrice)
		}
	}

	// Notify the strategy that live trading is about to begin.
	// This fires exactly once, after warmup and portfolio reset.
	if lh, ok := e.strategy.(strategy.LiveHandler); ok {
		livePositions := make([]strategy.Position, 0, len(positions))
		for _, pos := range positions {
			side := "flat"
			if pos.Qty > 0 {
				side = "long"
			} else if pos.Qty < 0 {
				side = "short"
			}
			livePositions = append(livePositions, strategy.Position{
				Symbol:     pos.Symbol,
				Qty:        pos.Qty,
				AvgCost:    pos.AvgEntryPrice,
				EntryPrice: pos.AvgEntryPrice,
				Side:       side,
			})
		}
		lh.OnLive(strategy.LiveContext{Positions: livePositions})
		e.logf("recovery: onLive called")
	}

	e.logf("recovery: complete — ready to trade")
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
func simulateFills(strat strategy.Strategy, port *portfolio.SimulatedPortfolio, orders []strategy.Order, tick strategy.Tick, eng *Engine) {
	for _, o := range orders {
		if o.OrderType != "market" && o.OrderType != "" {
			continue
		}

		fillPrice := tick.Close

		fill := strategy.Fill{
			Symbol:    o.Symbol,
			Side:      o.Side,
			Qty:       o.Qty,
			Price:     fillPrice,
			Timestamp: tick.Timestamp,
		}
		port.ApplyFill(fill)
		strat.SetPortfolio(port)
		strat.OnFill(fill)

		// Register bracket for simulation on subsequent bars.
		if eng != nil {
			if b := bracket.NewFromOrder(o, fillPrice); b != nil {
				eng.warmupBrackets = append(eng.warmupBrackets, *b)
			}
		}
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
