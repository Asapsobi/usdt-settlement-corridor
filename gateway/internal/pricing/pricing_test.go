package pricing_test

import (
	"errors"
	"testing"

	"gateway/internal/money"
	"gateway/internal/pricing"
)

func mustAmount(t *testing.T, s string) money.Amount {
	t.Helper()
	a, err := money.ParseDecimal(s)
	if err != nil {
		t.Fatalf("ParseDecimal(%q): %v", s, err)
	}
	return a
}

// TestComputeQuote_GoldenExample is c6-api-gateway-build-prompts.md §B's
// own golden test, findings-and-recommendation.md's worked example:
// $3,000 at STANDARD must produce exactly $7.50 fee, $1.80 network fee,
// $2,990.70 out. This must pass before anything else in C6 is trusted --
// see this package's own doc comment.
func TestComputeQuote_GoldenExample(t *testing.T) {
	q, err := pricing.ComputeQuote(pricing.Standard, mustAmount(t, "3000.000000"))
	if err != nil {
		t.Fatalf("ComputeQuote: %v", err)
	}
	if q.FeeUnits != mustAmount(t, "7.500000") {
		t.Errorf("FeeUnits = %s, want 7.500000", q.FeeUnits.Format())
	}
	if q.NetworkFeeUnits != mustAmount(t, "1.800000") {
		t.Errorf("NetworkFeeUnits = %s, want 1.800000", q.NetworkFeeUnits.Format())
	}
	if q.AmountOut != mustAmount(t, "2990.700000") {
		t.Errorf("AmountOut = %s, want 2990.700000", q.AmountOut.Format())
	}
}

// TestComputeQuote_BracketFlatRate confirms every bracket boundary
// applies its own flat rate to the WHOLE amount, per this package's own
// doc comment on why that -- not marginal application -- is what the
// golden test and its source actually require. One case per bracket,
// picked well inside each bracket to avoid floor/boundary ambiguity.
func TestComputeQuote_BracketFlatRate(t *testing.T) {
	cases := []struct {
		amountIn string
		wantFee  string
	}{
		{"500.000000", "2.000000"},       // under $1k: 40bp of 500 = 2.00
		{"5000.000000", "12.500000"},     // $1k-$10k: 25bp of 5000 = 12.50
		{"50000.000000", "60.000000"},    // $10k-$100k: 12bp of 50000 = 60.00
		{"500000.000000", "300.000000"},  // $100k-$1M: 6bp of 500000 = 300.00
		{"2000000.000000", "700.000000"}, // $1M+: 3.5bp of 2000000 = 700.00
	}
	for _, c := range cases {
		q, err := pricing.ComputeQuote(pricing.Standard, mustAmount(t, c.amountIn))
		if err != nil {
			t.Fatalf("ComputeQuote(%s): %v", c.amountIn, err)
		}
		if want := mustAmount(t, c.wantFee); q.FeeUnits != want {
			t.Errorf("ComputeQuote(%s).FeeUnits = %s, want %s", c.amountIn, q.FeeUnits.Format(), want.Format())
		}
	}
}

// TestComputeQuote_FloorApplies confirms the $1.80 floor wins over a
// bracket rate that would otherwise compute below it.
func TestComputeQuote_FloorApplies(t *testing.T) {
	// 40bp of $10 = $0.04, well under the $1.80 floor.
	q, err := pricing.ComputeQuote(pricing.Sweep, mustAmount(t, "10.000000"))
	if err != nil {
		t.Fatalf("ComputeQuote: %v", err)
	}
	if q.FeeUnits != mustAmount(t, "1.800000") {
		t.Errorf("FeeUnits = %s, want the 1.800000 floor", q.FeeUnits.Format())
	}
}

// TestComputeQuote_Property is C6.0's own acceptance criterion: for
// every tier and a spread of amountIn values (including exact bracket
// boundaries), feeUnits is never below the floor and amountOut is never
// negative for any amount large enough to be quotable at all.
func TestComputeQuote_Property(t *testing.T) {
	tiers := []pricing.Tier{pricing.Direct, pricing.Standard, pricing.Sweep}
	amounts := []string{
		"5.000000", "10.000000", "100.000000",
		"999.990000", "1000.000000", "1000.010000",
		"9999.990000", "10000.000000", "10000.010000",
		"99999.990000", "100000.000000", "100000.010000",
		"999999.990000", "1000000.000000", "1000000.010000",
		"5000000.000000", "50000000.000000",
	}
	for _, tier := range tiers {
		for _, amt := range amounts {
			amountIn := mustAmount(t, amt)
			q, err := pricing.ComputeQuote(tier, amountIn)
			if err != nil {
				if errors.Is(err, pricing.ErrAmountTooSmall) {
					continue // too small to cover fee+network_fee -- a valid rejection, not a property violation
				}
				t.Fatalf("ComputeQuote(%s, %s): %v", tier, amt, err)
			}
			if q.FeeUnits < mustAmount(t, "1.800000") {
				t.Errorf("ComputeQuote(%s, %s).FeeUnits = %s, below the 1.80 floor", tier, amt, q.FeeUnits.Format())
			}
			if q.AmountOut <= 0 {
				t.Errorf("ComputeQuote(%s, %s).AmountOut = %s, want positive", tier, amt, q.AmountOut.Format())
			}
			if q.AmountIn != q.FeeUnits+q.NetworkFeeUnits+q.AmountOut {
				t.Errorf("ComputeQuote(%s, %s): amount_in != fee+network_fee+amount_out (%s != %s+%s+%s)",
					tier, amt, q.AmountIn.Format(), q.FeeUnits.Format(), q.NetworkFeeUnits.Format(), q.AmountOut.Format())
			}
		}
	}
}

func TestComputeQuote_NonPositiveAmountRejected(t *testing.T) {
	for _, amt := range []string{"0.000000", "-1.000000"} {
		if _, err := pricing.ComputeQuote(pricing.Direct, mustAmount(t, amt)); !errors.Is(err, pricing.ErrNonPositiveAmount) {
			t.Errorf("ComputeQuote(%s): err = %v, want ErrNonPositiveAmount", amt, err)
		}
	}
}

func TestComputeQuote_UnknownTierRejected(t *testing.T) {
	if _, err := pricing.ComputeQuote(pricing.Tier("BOGUS"), mustAmount(t, "100.000000")); !errors.Is(err, pricing.ErrUnknownTier) {
		t.Errorf("err = %v, want ErrUnknownTier", err)
	}
}

func TestComputeQuote_AmountTooSmallRejected(t *testing.T) {
	// Direct's own network fee alone is $2.54; $1.80 (the floor fee)
	// plus that exceeds a $2 amount_in entirely.
	if _, err := pricing.ComputeQuote(pricing.Direct, mustAmount(t, "2.000000")); !errors.Is(err, pricing.ErrAmountTooSmall) {
		t.Errorf("err = %v, want ErrAmountTooSmall", err)
	}
}
