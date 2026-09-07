package money

import "testing"

func TestAmount_Format(t *testing.T) {
	tests := []struct {
		amount Amount
		want   string
	}{
		{24_500000, "24.500000"},
		{-24_500000, "-24.500000"},
		{1, "0.000001"},
		{-1, "-0.000001"},
		{0, "0.000000"},
	}
	for _, tc := range tests {
		if got := tc.amount.Format(); got != tc.want {
			t.Errorf("Amount(%d).Format() = %q, want %q", tc.amount, got, tc.want)
		}
	}
}

func TestParseDecimal_RoundTripsWithFormat(t *testing.T) {
	amounts := []Amount{0, 1, -1, 24_500000, -24_500000, 999999_999999}
	for _, a := range amounts {
		got, err := ParseDecimal(a.Format())
		if err != nil {
			t.Fatalf("ParseDecimal(%q): %v", a.Format(), err)
		}
		if got != a {
			t.Errorf("ParseDecimal(Format(%d)) = %d, want %d", a, got, a)
		}
	}
}

func TestParseDecimal_Table(t *testing.T) {
	tests := []struct {
		in      string
		want    Amount
		wantErr bool
	}{
		{"24.500000", 24_500000, false},
		{"24", 24_000000, false},
		{"-0.5", -500000, false},
		{"0.000001", 1, false},
		{"", 0, true},
		{"abc", 0, true},
		{"1.2.3", 0, true},
		{"1.0000001", 0, true}, // more than Decimals fractional digits
	}
	for _, tc := range tests {
		got, err := ParseDecimal(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseDecimal(%q): expected an error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDecimal(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseDecimal(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
