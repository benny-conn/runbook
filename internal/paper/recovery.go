package paper

import (
	"context"
	"fmt"
	"log"
	"time"

	"brandon-bot/internal/portfolio"
	"brandon-bot/internal/provider"
	"brandon-bot/internal/strategy"
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

	account, err := e.exec.GetAccount(ctx)
	if err != nil {
		return fmt.Errorf("getting account: %w", err)
	}

	positions, err := e.exec.GetPositions(ctx)
	if err != nil {
		return fmt.Errorf("getting positions: %w", err)
	}

	// Seed portfolio with real cash balance.
	e.portfolio = portfolio.NewSimulatedPortfolio(account.Cash)

	// Apply any existing open positions.
	for _, pos := range positions {
		e.portfolio.ApplyFill(strategy.Fill{
			Symbol: pos.Symbol,
			Side:   "buy",
			Qty:    pos.Qty,
			Price:  pos.AvgEntryPrice,
		})
		if pos.CurrentPrice > 0 {
			e.portfolio.UpdateMarketPrice(pos.Symbol, pos.CurrentPrice)
		}
	}

	log.Printf("recovery: portfolio seeded — cash=$%.2f equity=$%.2f open_positions=%d",
		e.portfolio.Cash(), e.portfolio.Equity(), len(positions))

	// Fetch enough recent bars to warm up the strategy's rolling indicators.
	end := time.Now()
	start := end.Add(-warmupWindow(e.config.Timeframe, e.config.WarmupBars))

	log.Printf("recovery: fetching %d bars of %s history for warm-up...", e.config.WarmupBars, e.config.Timeframe)

	bars, err := e.md.FetchBarsMulti(ctx, symbols, e.config.Timeframe, start, end)
	if err != nil {
		return fmt.Errorf("fetching warm-up bars: %w", err)
	}

	log.Printf("recovery: replaying %d bars through strategy (no orders placed)...", len(bars))

	for _, b := range bars {
		tick := provider.BarToTick(b)
		e.portfolio.UpdateMarketPrice(tick.Symbol, tick.Close)
		e.strategy.OnTick(tick, e.portfolio) // returned orders intentionally discarded
	}

	// If the strategy supports position injection, tell it what we currently hold.
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

// warmupWindow returns how far back to fetch historical bars.
// For daily bars we go back bars*2 calendar days.
// For sub-day bars we fetch 10 calendar days — always enough bars at any frequency.
func warmupWindow(timeframe string, bars int) time.Duration {
	if timeframe == "1d" {
		return time.Duration(bars*2) * 24 * time.Hour
	}
	return 10 * 24 * time.Hour
}
