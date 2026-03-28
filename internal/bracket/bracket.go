// Package bracket provides TP/SL bracket simulation shared by the live engine
// (warmup) and the backtest engine.
package bracket

import (
	"github.com/benny-conn/runbook/strategy"
)

// Pending tracks a TP/SL bracket awaiting execution.
type Pending struct {
	Symbol     string
	Side       string  // side of the ENTRY order ("buy" or "sell")
	Qty        float64
	TakeProfit float64 // absolute TP price (0 = none)
	StopLoss   float64 // absolute SL price (0 = none)
}

// NewFromOrder creates a Pending bracket from a filled entry order.
// Returns nil if the order has no TP/SL distances.
func NewFromOrder(order strategy.Order, fillPrice float64) *Pending {
	if order.TPDistance == 0 && order.SLDistance == 0 {
		return nil
	}

	b := &Pending{
		Symbol: order.Symbol,
		Side:   order.Side,
		Qty:    order.Qty,
	}
	if order.Side == "buy" {
		if order.TPDistance > 0 {
			b.TakeProfit = fillPrice + order.TPDistance
		}
		if order.SLDistance > 0 {
			b.StopLoss = fillPrice - order.SLDistance
		}
	} else {
		if order.TPDistance > 0 {
			b.TakeProfit = fillPrice - order.TPDistance
		}
		if order.SLDistance > 0 {
			b.StopLoss = fillPrice + order.SLDistance
		}
	}
	return b
}

// Trigger holds the result of a triggered bracket.
type Trigger struct {
	Side  string  // "buy" or "sell" — the exit side
	Price float64 // fill price (TP or SL level)
}

// Check evaluates this bracket against a bar's high/low.
// Returns a Trigger if the TP or SL would have been hit, nil otherwise.
// Only checks against the given symbol — caller should pre-filter or pass matching ticks.
func (b *Pending) Check(high, low float64) *Trigger {
	if b.Side == "buy" {
		// Long entry: TP if high >= TP, SL if low <= SL.
		if b.TakeProfit > 0 && high >= b.TakeProfit {
			return &Trigger{Side: "sell", Price: b.TakeProfit}
		}
		if b.StopLoss > 0 && low <= b.StopLoss {
			return &Trigger{Side: "sell", Price: b.StopLoss}
		}
	} else {
		// Short entry: TP if low <= TP, SL if high >= SL.
		if b.TakeProfit > 0 && low <= b.TakeProfit {
			return &Trigger{Side: "buy", Price: b.TakeProfit}
		}
		if b.StopLoss > 0 && high >= b.StopLoss {
			return &Trigger{Side: "buy", Price: b.StopLoss}
		}
	}
	return nil
}

// CheckAll evaluates all pending brackets for a given symbol against a bar's
// high/low. Triggered brackets are removed from the slice. Returns the
// remaining brackets and any triggered fills (caller must set Timestamp).
func CheckAll(brackets []Pending, symbol string, high, low float64) (remaining []Pending, fills []strategy.Fill) {
	for _, b := range brackets {
		if b.Symbol != symbol {
			remaining = append(remaining, b)
			continue
		}
		if t := b.Check(high, low); t != nil {
			fills = append(fills, strategy.Fill{
				Symbol: b.Symbol,
				Side:   t.Side,
				Qty:    b.Qty,
				Price:  t.Price,
			})
		} else {
			remaining = append(remaining, b)
		}
	}
	return
}
