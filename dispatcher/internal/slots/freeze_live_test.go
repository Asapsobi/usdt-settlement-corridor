//go:build live

// Requires real internet access to api.trongrid.io -- excluded from
// normal test runs (both `go test ./...` and the integration tag) by its
// own build tag, run explicitly via `go test -tags=live ./internal/slots/...`
// when someone wants to re-confirm this package's real HTTP shape still
// matches TronGrid's actual API, the same posture as every other
// "verified live, not just tested against a fake" claim in this module.
package slots_test

import (
	"context"
	"testing"

	"dispatcher/internal/slots"
)

func TestHTTPFreezeChecker_LiveNotBlacklisted(t *testing.T) {
	c := slots.NewHTTPFreezeChecker("https://api.trongrid.io")
	blacklisted, err := c.IsBlackListed(context.Background(), "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH")
	if err != nil {
		t.Fatalf("IsBlackListed: %v", err)
	}
	if blacklisted {
		t.Fatal("IsBlackListed = true for an ordinary, known-active address, want false")
	}
}
