package risk

import (
	"math"
	"testing"
)

func TestPositionSize(t *testing.T) {
	tests := []struct {
		name     string
		cash     float64
		fraction float64
		want     float64
	}{
		{"10% of 10k", 10000, 0.10, 1000},
		{"100% of 5k", 5000, 1.0, 5000},
		{"0% of 10k", 10000, 0.0, 0},
		{"50% of 0", 0, 0.50, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PositionSize(tt.cash, tt.fraction)
			if got != tt.want {
				t.Errorf("PositionSize(%v, %v) = %v, want %v", tt.cash, tt.fraction, got, tt.want)
			}
		})
	}
}

func TestQtyForDollarAmount(t *testing.T) {
	tests := []struct {
		name    string
		dollars float64
		price   float64
		want    float64
	}{
		{"1000 at 50", 1000, 50, 20},
		{"1000 at 33", 1000, 33, 1000.0 / 33.0},
		{"zero dollars", 0, 100, 0},
		{"zero price", 1000, 0, 0},
		{"negative price", 1000, -10, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := QtyForDollarAmount(tt.dollars, tt.price)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("QtyForDollarAmount(%v, %v) = %v, want %v", tt.dollars, tt.price, got, tt.want)
			}
		})
	}
}
