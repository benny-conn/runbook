package risk

// PositionSize returns the dollar amount to allocate given available cash and a fraction (0–1).
func PositionSize(cash, fraction float64) float64 {
	return cash * fraction
}

// QtyForDollarAmount returns how many shares to buy given a target dollar amount and current price.
func QtyForDollarAmount(dollars, price float64) float64 {
	if price <= 0 {
		return 0
	}
	return dollars / price
}
