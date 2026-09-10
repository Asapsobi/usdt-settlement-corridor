package customers_test

import (
	"strings"
	"testing"

	"gateway/internal/customers"
)

func TestGenerateAPIKey_UniqueAndPrefixed(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		key, err := customers.GenerateAPIKey()
		if err != nil {
			t.Fatalf("GenerateAPIKey: %v", err)
		}
		if !strings.HasPrefix(key, "sk_live_") {
			t.Fatalf("GenerateAPIKey() = %q, want sk_live_ prefix", key)
		}
		if seen[key] {
			t.Fatalf("GenerateAPIKey produced a duplicate: %q", key)
		}
		seen[key] = true
	}
}

func TestHashAPIKey_DeterministicAndDistinct(t *testing.T) {
	h1 := customers.HashAPIKey("sk_live_abc")
	h2 := customers.HashAPIKey("sk_live_abc")
	if h1 != h2 {
		t.Fatalf("HashAPIKey is not deterministic: %q != %q", h1, h2)
	}
	h3 := customers.HashAPIKey("sk_live_xyz")
	if h1 == h3 {
		t.Fatal("HashAPIKey produced the same hash for two different keys")
	}
	if h1 == "sk_live_abc" {
		t.Fatal("HashAPIKey returned the raw key unchanged -- not actually hashed")
	}
}
