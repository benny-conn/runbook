package strategies

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/benny-conn/brandon-bot/strategy"
)

// orbPhase is the daily state machine for FiveMinuteORB.
type orbPhase int

const (
	orbPhaseWaiting    orbPhase = iota // before 9:30 AM ET
	orbPhaseCollecting                  // 9:30:00–9:34:59 ET: building the opening range
	orbPhaseArmed                       // 9:35:00+ ET: watching for a level break
	orbPhaseEntering                    // entry order submitted, awaiting fill confirmation
	orbPhaseInTrade                     // position open, monitoring TP/SL
	orbPhaseDone                        // trade fired (or range skipped) — done for the day
)

// FiveMinuteORBConfig holds all tuneable parameters for the strategy.
// Zero values use built-in defaults suitable for NQ/ES micro futures.
type FiveMinuteORBConfig struct {
	// Qty is the number of contracts (or shares) per trade. Default: 1.
	Qty float64 `json:"qty"`

	// TickSize is the minimum price increment for the instrument.
	// NQ/ES full = 0.25, MNQ/MES micro = 0.25. Default: 0.25.
	TickSize float64 `json:"tick_size"`

	// TakeProfitTicks is how far from entry to target in ticks.
	// Default: 20 ticks (5 points on NQ = $50/MNQ, $100/NQ per contract).
	TakeProfitTicks int `json:"take_profit_ticks"`

	// StopLossTicks is how far back through the broken level to place the stop.
	// Default: 8 ticks (2 points on NQ). If price returns through the level by
	// this amount the trade is considered invalidated.
	StopLossTicks int `json:"stop_loss_ticks"`

	// MaxRangeTicks skips the trade if the opening range is wider than this.
	// A large opening candle suggests volatility that makes the level unreliable.
	// Default: 0 (no filter). Reasonable starting value: 60 ticks (15 NQ points).
	MaxRangeTicks int `json:"max_range_ticks"`
}

// FiveMinuteORB implements Brandon's Five-Minute Opening Range Breakout strategy.
//
// Each trading day:
//  1. At 9:30 AM ET, begin tracking the high and low of the first 5-minute candle.
//  2. At 9:35 AM ET, the opening range is set (ORH = range high, ORL = range low).
//  3. Enter long immediately when price breaks above ORH.
//     Enter short immediately when price breaks below ORL.
//  4. Exit at a fixed take-profit distance from entry, or if price reverses
//     back through the level by StopLossTicks.
//  5. One trade per day — the strategy is done after the first trade fires.
//
// On 1-minute bars the entry triggers on the close of the first bar that
// breaks the level. On 1-second bars or tick data (via TradeSubscriber) the
// entry triggers within one second of the break — the intended live use case.
//
// Defaults are calibrated for NQ/MNQ futures. Adjust TickSize and
// TakeProfitTicks/StopLossTicks for other instruments.
type FiveMinuteORB struct {
	cfg FiveMinuteORBConfig

	phase      orbPhase
	orbHigh    float64 // opening range high (set at 9:35)
	orbLow     float64 // opening range low  (set at 9:35)
	entrySide  string  // "buy" or "sell"
	entryPrice float64 // filled entry price (set in OnFill)
	entryQty   float64 // filled qty         (set in OnFill)

	lastDate string // "2006-01-02" in ET — used to detect a new trading day
	et       *time.Location
}

// NewFiveMinuteORB creates a FiveMinuteORB strategy with the given config.
// Any zero-value config fields are replaced with sensible defaults.
func NewFiveMinuteORB(cfg FiveMinuteORBConfig) *FiveMinuteORB {
	if cfg.Qty == 0 {
		cfg.Qty = 1
	}
	if cfg.TickSize == 0 {
		cfg.TickSize = 0.25
	}
	if cfg.TakeProfitTicks == 0 {
		cfg.TakeProfitTicks = 20
	}
	if cfg.StopLossTicks == 0 {
		cfg.StopLossTicks = 8
	}
	et, _ := time.LoadLocation("America/New_York")
	return &FiveMinuteORB{cfg: cfg, phase: orbPhaseWaiting, et: et}
}

func (f *FiveMinuteORB) Name() string { return "five_min_orb" }

// OnTick is called on every completed bar. It drives the state machine and
// triggers entries/exits based on bar highs/lows.
func (f *FiveMinuteORB) OnTick(tick strategy.Tick, portfolio strategy.Portfolio) []strategy.Order {
	f.maybeResetDay(tick.Timestamp)

	tET := tick.Timestamp.In(f.et)
	h, m := tET.Hour(), tET.Minute()

	switch f.phase {
	case orbPhaseWaiting:
		if h == 9 && m >= 30 {
			f.phase = orbPhaseCollecting
			f.orbHigh = tick.High
			f.orbLow = tick.Low
		}

	case orbPhaseCollecting:
		if h == 9 && m < 35 {
			// Expand opening range as more bars arrive in the 9:30–9:34 window.
			if tick.High > f.orbHigh {
				f.orbHigh = tick.High
			}
			if tick.Low < f.orbLow {
				f.orbLow = tick.Low
			}
		} else {
			// First bar at or after 9:35 — opening range is finalised.
			f.transitionToArmedOrDone()
		}

	case orbPhaseArmed:
		return f.evalBreak(tick.Symbol, tick.High, tick.Low)

	case orbPhaseInTrade:
		return f.evalExits(tick.Symbol, tick.High, tick.Low)
	}

	return nil
}

// OnTrade implements strategy.TradeSubscriber.
// When the engine is running with a trade-level subscription (e.g. --timeframe=1s),
// this fires on every individual trade print for sub-second entry and exit precision.
// OnTick is still called for completed bars; implement it as a no-op if not needed,
// but here it drives the state machine so both work together correctly.
func (f *FiveMinuteORB) OnTrade(trade strategy.Trade, portfolio strategy.Portfolio) []strategy.Order {
	f.maybeResetDay(trade.Timestamp)
	switch f.phase {
	case orbPhaseArmed:
		// A trade print crossing the level is our entry trigger.
		return f.evalBreak(trade.Symbol, trade.Price, trade.Price)
	case orbPhaseInTrade:
		return f.evalExits(trade.Symbol, trade.Price, trade.Price)
	}
	return nil
}

// OnFill records the confirmed fill price/qty and advances the state machine.
func (f *FiveMinuteORB) OnFill(fill strategy.Fill) {
	switch f.phase {
	case orbPhaseEntering:
		// Entry fill confirmed — record details and start monitoring.
		f.entryPrice = fill.Price
		f.entryQty = fill.Qty
		f.entrySide = fill.Side
		f.phase = orbPhaseInTrade
	case orbPhaseInTrade:
		// Exit fill confirmed — done for the day.
		f.phase = orbPhaseDone
	}
}

// SeedPosition implements engine.PositionSeeder.
// Called on startup if the bot restarts while already holding a position,
// so the strategy immediately begins monitoring for TP/SL rather than trying
// to enter a new trade.
func (f *FiveMinuteORB) SeedPosition(symbol string, qty, avgCost float64) {
	if qty == 0 {
		return
	}
	f.entryPrice = avgCost
	f.entryQty = qty
	f.phase = orbPhaseInTrade
	if qty > 0 {
		f.entrySide = "buy"
	} else {
		f.entrySide = "sell"
		f.entryQty = -qty // store positive, track direction via entrySide
	}
}

// Configure implements strategy.Configurable.
// Fields present in the JSON override constructor defaults; absent fields keep their values.
func (f *FiveMinuteORB) Configure(data []byte) error {
	return json.Unmarshal(data, &f.cfg)
}

// — internal helpers —

// evalBreak checks whether the current high or low has broken the opening range
// and returns a market entry order if so.
func (f *FiveMinuteORB) evalBreak(symbol string, high, low float64) []strategy.Order {
	if high > f.orbHigh {
		f.phase = orbPhaseEntering // lock out further entries until fill arrives
		return []strategy.Order{{
			Symbol:    symbol,
			Side:      "buy",
			Qty:       f.cfg.Qty,
			OrderType: "market",
			Reason:    fmt.Sprintf("ORB long: broke above %.2f (range %.2f–%.2f)", f.orbHigh, f.orbLow, f.orbHigh),
		}}
	}
	if low < f.orbLow {
		f.phase = orbPhaseEntering
		return []strategy.Order{{
			Symbol:    symbol,
			Side:      "sell",
			Qty:       f.cfg.Qty,
			OrderType: "market",
			Reason:    fmt.Sprintf("ORB short: broke below %.2f (range %.2f–%.2f)", f.orbLow, f.orbLow, f.orbHigh),
		}}
	}
	return nil
}

// evalExits checks take-profit and stop-loss conditions once in a trade.
// Waits until entryPrice is populated from OnFill before checking.
func (f *FiveMinuteORB) evalExits(symbol string, high, low float64) []strategy.Order {
	if f.entryPrice == 0 {
		return nil // fill hasn't arrived yet
	}

	tp := float64(f.cfg.TakeProfitTicks) * f.cfg.TickSize
	sl := float64(f.cfg.StopLossTicks) * f.cfg.TickSize

	switch f.entrySide {
	case "buy":
		tpLevel := f.entryPrice + tp
		slLevel := f.orbHigh - sl // SL is back through the broken level
		if high >= tpLevel {
			f.phase = orbPhaseDone
			return []strategy.Order{{
				Symbol:    symbol,
				Side:      "sell",
				Qty:       f.entryQty,
				OrderType: "market",
				Reason:    fmt.Sprintf("ORB TP hit: %.2f", tpLevel),
			}}
		}
		if low <= slLevel {
			f.phase = orbPhaseDone
			return []strategy.Order{{
				Symbol:    symbol,
				Side:      "sell",
				Qty:       f.entryQty,
				OrderType: "market",
				Reason:    fmt.Sprintf("ORB SL hit: %.2f (back through level)", slLevel),
			}}
		}

	case "sell":
		tpLevel := f.entryPrice - tp
		slLevel := f.orbLow + sl // SL is back through the broken level
		if low <= tpLevel {
			f.phase = orbPhaseDone
			return []strategy.Order{{
				Symbol:    symbol,
				Side:      "buy",
				Qty:       f.entryQty,
				OrderType: "market",
				Reason:    fmt.Sprintf("ORB TP hit: %.2f", tpLevel),
			}}
		}
		if high >= slLevel {
			f.phase = orbPhaseDone
			return []strategy.Order{{
				Symbol:    symbol,
				Side:      "buy",
				Qty:       f.entryQty,
				OrderType: "market",
				Reason:    fmt.Sprintf("ORB SL hit: %.2f (back through level)", slLevel),
			}}
		}
	}

	return nil
}

// transitionToArmedOrDone finalises the opening range and either arms the strategy
// or marks it done for the day if the range is too wide (MaxRangeTicks filter).
func (f *FiveMinuteORB) transitionToArmedOrDone() {
	if f.cfg.MaxRangeTicks > 0 {
		rangeTicks := int((f.orbHigh - f.orbLow) / f.cfg.TickSize)
		if rangeTicks > f.cfg.MaxRangeTicks {
			f.phase = orbPhaseDone
			return
		}
	}
	f.phase = orbPhaseArmed
}

// maybeResetDay resets all intra-day state at midnight ET so the strategy is
// ready for a fresh session each morning without restarting the bot.
func (f *FiveMinuteORB) maybeResetDay(t time.Time) {
	d := t.In(f.et).Format("2006-01-02")
	if f.lastDate == "" {
		f.lastDate = d
		return
	}
	if d == f.lastDate {
		return
	}
	f.lastDate = d
	f.phase = orbPhaseWaiting
	f.orbHigh = 0
	f.orbLow = 0
	f.entryPrice = 0
	f.entrySide = ""
	f.entryQty = 0
}
