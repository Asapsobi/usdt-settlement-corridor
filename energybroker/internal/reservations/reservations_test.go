package reservations

import (
	"errors"
	"testing"
	"time"
)

func validRequest() Request {
	return Request{
		IdempotencyKey: "dispatch:order-1:1",
		ExternalID:     "order-1",
		TargetAddress:  "TSlotAddress00000000000000000001",
		EnergyUnits:    1000,
		Tier:           "STANDARD",
		Deadline:       time.Now().Add(time.Minute),
	}
}

func TestRequest_Validate(t *testing.T) {
	if err := validRequest().validate(); err != nil {
		t.Fatalf("a well-formed request failed validation: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(r *Request)
	}{
		{"empty idempotency key", func(r *Request) { r.IdempotencyKey = "" }},
		{"empty external_id", func(r *Request) { r.ExternalID = "" }},
		{"empty target_address", func(r *Request) { r.TargetAddress = "" }},
		{"zero energy_units", func(r *Request) { r.EnergyUnits = 0 }},
		{"negative energy_units", func(r *Request) { r.EnergyUnits = -1 }},
		{"invalid tier", func(r *Request) { r.Tier = "EXPRESS" }},
		{"zero deadline", func(r *Request) { r.Deadline = time.Time{} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := validRequest()
			tc.mutate(&req)
			if err := req.validate(); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("validate() error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestJoinVendorNames(t *testing.T) {
	tests := []struct {
		name string
		seen map[string]bool
		want string
	}{
		{"single vendor", map[string]bool{"tronsell": true}, "tronsell"},
		{"multiple vendors sorted deterministically", map[string]bool{"netts": true, "catfee": true}, "catfee,netts"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinVendorNames(tc.seen); got != tc.want {
				t.Errorf("joinVendorNames(%v) = %q, want %q", tc.seen, got, tc.want)
			}
		})
	}
}
