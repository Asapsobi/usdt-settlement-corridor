package provider_test

import (
	"context"
	"testing"

	"screening/internal/provider"
)

func TestAlwaysCleanProvider_NeverFlags(t *testing.T) {
	p := provider.AlwaysCleanProvider{}
	for _, addr := range []string{"", "TFakeAddress1", "0xdeadbeef", "an address a real vendor would flag"} {
		v, err := p.Screen(context.Background(), addr)
		if err != nil {
			t.Fatalf("Screen(%q): %v", addr, err)
		}
		if v.Flagged {
			t.Fatalf("Screen(%q).Flagged = true, want false", addr)
		}
		if v.RiskScore != 0 {
			t.Fatalf("Screen(%q).RiskScore = %v, want 0", addr, v.RiskScore)
		}
		if v.ProviderName != provider.AlwaysCleanProviderName {
			t.Fatalf("Screen(%q).ProviderName = %q, want %q", addr, v.ProviderName, provider.AlwaysCleanProviderName)
		}
	}
}

func TestAlwaysCleanProvider_RespectsCancelledContext(t *testing.T) {
	p := provider.AlwaysCleanProvider{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Screen(ctx, "TAnyAddress"); err == nil {
		t.Fatal("Screen with a cancelled context: want an error, got nil")
	}
}
