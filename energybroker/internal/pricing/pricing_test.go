package pricing

import (
	"testing"

	"energybroker/internal/provider"
)

func TestUnderCeiling(t *testing.T) {
	tests := []struct {
		name    string
		price   float64
		ceiling float64
		want    bool
	}{
		{"strictly below ceiling", 24.0, 25.7, true},
		{"exactly at ceiling is under it, not above it", 25.7, 25.7, true},
		{"strictly above ceiling", 25.701, 25.7, false},
		{"zero price is always under a positive ceiling", 0, 25.7, true},
		{"zero ceiling rejects any positive price", 0.001, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := provider.Quote{PricePerUnitSun: tc.price}
			if got := UnderCeiling(q, tc.ceiling); got != tc.want {
				t.Errorf("UnderCeiling(price=%v, ceiling=%v) = %v, want %v", tc.price, tc.ceiling, got, tc.want)
			}
		})
	}
}
