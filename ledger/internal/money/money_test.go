package money

// Deliberately stdlib-only, including this test file: the C1.0 build spec
// requires "no file in internal/money imports anything outside the
// standard library," with no carve-out for tests, so this package uses
// plain testing.T assertions rather than the project's usual testify.

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"testing"
)

func TestAssetDecimals(t *testing.T) {
	cases := []struct {
		asset    Asset
		decimals int
	}{
		{USDT_BEP20, 6},
		{USDT_TRC20, 6},
		{TRX, 6},
		{BNB, 9},
	}
	for _, c := range cases {
		got, err := c.asset.Decimals()
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", c.asset, err)
		}
		if got != c.decimals {
			t.Errorf("%s: got %d decimals, want %d", c.asset, got, c.decimals)
		}
		if !c.asset.Valid() {
			t.Errorf("%s: Valid() = false, want true", c.asset)
		}
	}

	_, err := Asset("DOGE").Decimals()
	if !errors.Is(err, ErrUnknownAsset) {
		t.Errorf("Decimals() on unknown asset: got %v, want ErrUnknownAsset", err)
	}
	if Asset("DOGE").Valid() {
		t.Error("Asset(\"DOGE\").Valid() = true, want false")
	}
	if Asset("").Valid() {
		t.Error("Asset(\"\").Valid() = true, want false")
	}
}

func TestAddRejectsDifferentAssets(t *testing.T) {
	a := Amount{Asset: USDT_BEP20, Units: 100}
	b := Amount{Asset: USDT_TRC20, Units: 100}
	if _, err := a.Add(b); !errors.Is(err, ErrAssetMismatch) {
		t.Errorf("Add across assets: got %v, want ErrAssetMismatch", err)
	}
}

func TestZeroValueAmountRejectedEverywhere(t *testing.T) {
	zero := Amount{}
	nonZero := Amount{Asset: TRX, Units: 1}

	if _, err := zero.Add(nonZero); !errors.Is(err, ErrZeroValueAmount) {
		t.Errorf("zero.Add(nonZero): got %v, want ErrZeroValueAmount", err)
	}
	if _, err := nonZero.Add(zero); !errors.Is(err, ErrZeroValueAmount) {
		t.Errorf("nonZero.Add(zero): got %v, want ErrZeroValueAmount", err)
	}
	if _, err := zero.Sub(nonZero); !errors.Is(err, ErrZeroValueAmount) {
		t.Errorf("zero.Sub(nonZero): got %v, want ErrZeroValueAmount", err)
	}
	if _, err := zero.Neg(); !errors.Is(err, ErrZeroValueAmount) {
		t.Errorf("zero.Neg(): got %v, want ErrZeroValueAmount", err)
	}
	if _, err := Format(zero); !errors.Is(err, ErrZeroValueAmount) {
		t.Errorf("Format(zero): got %v, want ErrZeroValueAmount", err)
	}
}

func TestNegOverflowAtMinInt64(t *testing.T) {
	a := Amount{Asset: TRX, Units: math.MinInt64}
	if _, err := a.Neg(); !errors.Is(err, ErrOverflow) {
		t.Errorf("Neg(MinInt64): got %v, want ErrOverflow", err)
	}
}

func TestAddOverflow(t *testing.T) {
	a := Amount{Asset: TRX, Units: math.MaxInt64}
	b := Amount{Asset: TRX, Units: 1}
	if _, err := a.Add(b); !errors.Is(err, ErrOverflow) {
		t.Errorf("MaxInt64 + 1: got %v, want ErrOverflow", err)
	}

	c := Amount{Asset: TRX, Units: math.MinInt64}
	d := Amount{Asset: TRX, Units: -1}
	if _, err := c.Add(d); !errors.Is(err, ErrOverflow) {
		t.Errorf("MinInt64 + -1: got %v, want ErrOverflow", err)
	}
}

func TestSubUnknownAsset(t *testing.T) {
	a := Amount{Asset: Asset("NOPE"), Units: 1}
	b := Amount{Asset: TRX, Units: 1}
	if _, err := a.Sub(b); !errors.Is(err, ErrUnknownAsset) {
		t.Errorf("Sub with unknown asset: got %v, want ErrUnknownAsset", err)
	}
}

// TestAddSubProperty is the C1.0-mandated property test: for 100k random
// int64 pairs, Add/Sub must either return a correct result or an overflow
// error -- never a wrong number, never a panic. math/big (standard
// library) is the independent oracle; it is used only here, never in the
// money package's own implementation, which is exactly the "no big.Rat in
// the implementation" rule the build spec draws.
func TestAddSubProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	const iterations = 100_000

	for i := 0; i < iterations; i++ {
		x := randInt64(rng)
		y := randInt64(rng)

		a := Amount{Asset: TRX, Units: x}
		b := Amount{Asset: TRX, Units: y}

		wantSum := new(big.Int).Add(big.NewInt(x), big.NewInt(y))
		gotSum, err := a.Add(b)
		checkAgainstOracle(t, wantSum, gotSum.Units, err, "Add", x, y)

		wantDiff := new(big.Int).Sub(big.NewInt(x), big.NewInt(y))
		gotDiff, err := a.Sub(b)
		checkAgainstOracle(t, wantDiff, gotDiff.Units, err, "Sub", x, y)
	}
}

func checkAgainstOracle(t *testing.T, want *big.Int, got int64, err error, op string, x, y int64) {
	t.Helper()
	if want.IsInt64() {
		if err != nil {
			t.Fatalf("%s(%d, %d): expected result %s, got unexpected error %v", op, x, y, want, err)
		}
		if got != want.Int64() {
			t.Fatalf("%s(%d, %d): expected %s, got %d", op, x, y, want, got)
		}
		return
	}
	if err == nil {
		t.Fatalf("%s(%d, %d): expected overflow error (true result %s does not fit int64), got %d", op, x, y, want, got)
	}
	if !errors.Is(err, ErrOverflow) {
		t.Fatalf("%s(%d, %d): got error %v, want ErrOverflow", op, x, y, err)
	}
}

func randInt64(rng *rand.Rand) int64 {
	// Bias toward the boundaries (near 0, near MinInt64/MaxInt64) as well as
	// uniform values, since boundary cases are where overflow bugs live.
	switch rng.Intn(4) {
	case 0:
		return rng.Int63() - rng.Int63() // roughly uniform over full range
	case 1:
		return math.MaxInt64 - int64(rng.Intn(1000))
	case 2:
		return math.MinInt64 + int64(rng.Intn(1000))
	default:
		return int64(rng.Intn(2000)) - 1000
	}
}

func TestParseDecimalTable(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		asset   Asset
		wantErr error
		want    int64 // only checked when wantErr is nil
	}{
		{"too many decimals", "1.0000001", USDT_TRC20, ErrTooManyDecimals, 0},
		{"scientific notation", "1e5", USDT_TRC20, ErrInvalidDecimal, 0},
		{"empty string", "", USDT_TRC20, ErrInvalidDecimal, 0},
		{"negative is legal", "-0.5", TRX, nil, -500000},
		{"whole number", "12", USDT_BEP20, nil, 12_000000},
		{"exact precision", "2990.700000", USDT_TRC20, nil, 2990700000},
		{"leading dot", ".5", TRX, nil, 500000},
		{"double dot", "1..5", TRX, ErrInvalidDecimal, 0},
		{"letters", "12a.5", TRX, ErrInvalidDecimal, 0},
		{"unknown asset", "1.0", Asset("NOPE"), ErrUnknownAsset, 0},
		{"bnb 9 decimals ok", "0.000000001", BNB, nil, 1},
		{"bnb 10 decimals rejected", "0.0000000001", BNB, ErrTooManyDecimals, 0},
		{"plus sign", "+5", TRX, nil, 5_000000},
		{"bare sign", "-", TRX, ErrInvalidDecimal, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseDecimal(c.input, c.asset)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("ParseDecimal(%q, %s): got error %v, want %v", c.input, c.asset, err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDecimal(%q, %s): unexpected error: %v", c.input, c.asset, err)
			}
			if got.Units != c.want {
				t.Fatalf("ParseDecimal(%q, %s): got %d units, want %d", c.input, c.asset, got.Units, c.want)
			}
		})
	}
}

// TestFormatParseRoundTrip covers the round-trip acceptance criterion:
// Format(Parse(x)) == x, for generated canonical decimal strings (i.e.
// strings already carrying exactly the asset's fixed number of fractional
// digits, which is the only form Format ever produces).
func TestFormatParseRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	assets := []Asset{USDT_BEP20, USDT_TRC20, TRX, BNB}

	for i := 0; i < 1000; i++ {
		asset := assets[rng.Intn(len(assets))]
		decimals, err := asset.Decimals()
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", asset, err)
		}

		units := randInt64(rng)
		intPart := units / pow10(decimals)
		fracUnits := units % pow10(decimals)
		if fracUnits < 0 {
			fracUnits = -fracUnits
		}
		sign := ""
		if units < 0 && intPart == 0 {
			sign = "-"
		}
		x := fmt.Sprintf("%s%d.%0*d", sign, intPart, decimals, fracUnits)

		parsed, err := ParseDecimal(x, asset)
		if err != nil {
			t.Fatalf("parsing generated string %q: %v", x, err)
		}

		formatted, err := Format(parsed)
		if err != nil {
			t.Fatalf("Format(%+v): unexpected error: %v", parsed, err)
		}
		if formatted != x {
			t.Fatalf("round trip: got %q, want %q", formatted, x)
		}
	}
}

func pow10(n int) int64 {
	r := int64(1)
	for i := 0; i < n; i++ {
		r *= 10
	}
	return r
}
