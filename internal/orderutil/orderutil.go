// Package orderutil provides shared order validation and construction
// utilities used by both the backtest and live engines.
package orderutil

import "github.com/benny-conn/brandon-bot/strategy"

// RejectFunc is called with a human-readable reason when an order is rejected.
type RejectFunc func(reason string)

// ValidateOrders filters out invalid orders. If maxContracts > 0, order qty is
// capped to that value. onReject is called for each dropped order; pass nil to
// suppress logging.
func ValidateOrders(orders []strategy.Order, maxContracts int, onReject RejectFunc) []strategy.Order {
	valid := make([]strategy.Order, 0, len(orders))
	for _, o := range orders {
		if o.Symbol == "" {
			if onReject != nil {
				onReject("dropping order with empty symbol")
			}
			continue
		}
		if o.Qty <= 0 {
			if onReject != nil {
				onReject("dropping " + o.Side + " " + o.Symbol + " order with non-positive qty")
			}
			continue
		}
		if maxContracts > 0 && o.Qty > float64(maxContracts) {
			o.Qty = float64(maxContracts)
		}
		valid = append(valid, o)
	}
	return valid
}

// BuildFlattenOrders creates market orders to close every open position.
func BuildFlattenOrders(positions []strategy.Position, reason string) []strategy.Order {
	var orders []strategy.Order
	for _, pos := range positions {
		if pos.Qty == 0 {
			continue
		}
		side := "sell"
		qty := pos.Qty
		if pos.Qty < 0 {
			side = "buy"
			qty = -pos.Qty
		}
		orders = append(orders, strategy.Order{
			Symbol:    pos.Symbol,
			Side:      side,
			Qty:       qty,
			OrderType: "market",
			Reason:    reason,
		})
	}
	return orders
}
