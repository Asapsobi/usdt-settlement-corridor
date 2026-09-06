package orders

import (
	"errors"
	"testing"
	"time"

	"ledger/internal/journal"
	"ledger/internal/money"
)

func validCreateParams() CreateParams {
	now := time.Now()
	return CreateParams{
		ExternalID:       "ext-1",
		CustomerID:       "cust-1",
		Tier:             Standard,
		AmountIn:         money.Amount{Asset: money.USDT_BEP20, Units: 3000_000000},
		AmountOut:        money.Amount{Asset: money.USDT_TRC20, Units: 2990_700000},
		FeeUnits:         money.Amount{Asset: money.USDT_TRC20, Units: 7_500000},
		NetworkFeeUnits:  money.Amount{Asset: money.USDT_TRC20, Units: 1_800000},
		RecipientAddress: "T-recipient",
		QuotedAt:         now,
		QuoteExpiresAt:   now.Add(90 * time.Second),
	}
}

// TestCreateParamsValidateValidPasses is the control: every rejection
// case below starts from these exact params and breaks exactly one
// field, so a failure here would mean the baseline itself is wrong, not
// that validate() is too strict.
func TestCreateParamsValidateValidPasses(t *testing.T) {
	if err := validCreateParams().validate(); err != nil {
		t.Fatalf("valid params rejected: %v", err)
	}
}

// TestCreateParamsValidate exercises every branch of validate() directly.
// None of these had a test before -- every existing integration test
// only ever exercises the accept path, since it needs a valid order to
// get anywhere.
func TestCreateParamsValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CreateParams)
	}{
		{"empty external_id", func(p *CreateParams) { p.ExternalID = "" }},
		{"empty customer_id", func(p *CreateParams) { p.CustomerID = "" }},
		{"invalid tier", func(p *CreateParams) { p.Tier = Tier("BOGUS") }},
		{"amount_in wrong asset", func(p *CreateParams) { p.AmountIn.Asset = money.USDT_TRC20 }},
		{"amount_out wrong asset", func(p *CreateParams) { p.AmountOut.Asset = money.USDT_BEP20 }},
		{"fee_units wrong asset", func(p *CreateParams) { p.FeeUnits.Asset = money.USDT_BEP20 }},
		{"network_fee_units wrong asset", func(p *CreateParams) { p.NetworkFeeUnits.Asset = money.USDT_BEP20 }},
		{"empty recipient_address", func(p *CreateParams) { p.RecipientAddress = "" }},
		{"zero quoted_at", func(p *CreateParams) { p.QuotedAt = time.Time{} }},
		{"quote_expires_at equal to quoted_at", func(p *CreateParams) { p.QuoteExpiresAt = p.QuotedAt }},
		{"quote_expires_at before quoted_at", func(p *CreateParams) { p.QuoteExpiresAt = p.QuotedAt.Add(-time.Second) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := validCreateParams()
			c.mutate(&p)
			err := p.validate()
			if !errors.Is(err, ErrInvalidParams) {
				t.Fatalf("validate() = %v, want ErrInvalidParams", err)
			}
		})
	}
}

func validTransitionParams() TransitionParams {
	return TransitionParams{Actor: "test:actor", Reason: "test reason", OccurredAt: time.Now()}
}

func TestTransitionParamsValidateValidPasses(t *testing.T) {
	if err := validTransitionParams().validate(); err != nil {
		t.Fatalf("valid params rejected: %v", err)
	}
}

// TestTransitionParamsValidate covers the same gap as
// TestCreateParamsValidate, for orders.Transition's own params. The
// build spec's C1.4 acceptance criterion for a hold/release transition
// is specifically "requires actor" -- this is the test proving that,
// rather than every transition test incidentally passing a non-empty
// actor and never checking what happens without one.
func TestTransitionParamsValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*TransitionParams)
	}{
		{"empty actor", func(p *TransitionParams) { p.Actor = "" }},
		{"empty reason", func(p *TransitionParams) { p.Reason = "" }},
		{"zero occurred_at", func(p *TransitionParams) { p.OccurredAt = time.Time{} }},
		{"both Entry and EntryID set", func(p *TransitionParams) {
			p.Entry = &journal.EntryRequest{}
			id := int64(1)
			p.EntryID = &id
		}},
		{"empty sender_address", func(p *TransitionParams) {
			empty := ""
			p.SenderAddress = &empty
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := validTransitionParams()
			c.mutate(&p)
			err := p.validate()
			if !errors.Is(err, ErrInvalidParams) {
				t.Fatalf("validate() = %v, want ErrInvalidParams", err)
			}
		})
	}
}

func TestCursor_RoundTrip(t *testing.T) {
	c := Cursor{UpdatedAt: time.Date(2026, 9, 6, 12, 0, 0, 123456789, time.UTC), ID: 42}
	parsed, err := ParseCursor(c.String())
	if err != nil {
		t.Fatalf("ParseCursor(%q): %v", c.String(), err)
	}
	if !parsed.UpdatedAt.Equal(c.UpdatedAt) || parsed.ID != c.ID {
		t.Fatalf("ParseCursor round-trip: got %+v, want %+v", parsed, c)
	}
}

func TestCursor_ParseRejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"not-valid-base64!!!",
		"bm8tcGlwZS1oZXJl", // valid base64, but decodes to "no-pipe-here" (no "|")
		"MjAyNi0wOS0wNlQxMjowMDowMFp8bm90LWFuLWlk", // "2026-09-06T12:00:00Z|not-an-id"
	}
	for _, s := range cases {
		if _, err := ParseCursor(s); !errors.Is(err, ErrInvalidParams) {
			t.Errorf("ParseCursor(%q) = %v, want ErrInvalidParams", s, err)
		}
	}
}
