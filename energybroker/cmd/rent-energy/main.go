// Command rent-energy delegates real TRON energy to an arbitrary
// receiver address, directly against a single named vendor -- for an
// operator who needs energy at a specific address that isn't one of
// the fixed payout slots internal/buffer provisions against (e.g.
// sweeping USDT off a one-off wallet that has no energy of its own).
// Never routed through internal/reservations or internal/buffer: those
// exist for C1 order flow against known payout slots, not this.
//
// Usage:
//
//	CATFEE_API_KEY=... CATFEE_API_SECRET=... \
//	  rent-energy -provider catfee -receiver THsJ1H5... -quantity 65000
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"energybroker/internal/provider"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	providerName := flag.String("provider", "catfee", "vendor name: catfee, netts, or tronsell")
	receiver := flag.String("receiver", "", "TRON address to delegate energy to (required)")
	quantity := flag.Int64("quantity", 65000, "energy units to delegate (catfee's own documented minimum is 65000)")
	duration := flag.Duration("duration", time.Hour, "how long the delegation should last -- vendors may only honor their own fixed duration regardless of this value (see each provider's own Delegate doc comment)")
	flag.Parse()

	if *receiver == "" {
		return fmt.Errorf("rent-energy: -receiver is required")
	}

	prov, err := providerFromFlag(*providerName)
	if err != nil {
		return err
	}

	ctx := context.Background()
	delegation, err := prov.Delegate(ctx, *receiver, *quantity, *duration)
	if err != nil {
		return fmt.Errorf("rent-energy: %w", err)
	}

	fmt.Printf("ok   delegated %d energy to %s via %s\n", delegation.EnergyUnits, delegation.TargetAddress, delegation.ProviderName)
	fmt.Printf("     delegation_id=%s cost_trx=%s expires_at=%s\n",
		delegation.ID, delegation.CostTRX.Format(), delegation.ExpiresAt.Format(time.RFC3339))
	return nil
}

// providerFromFlag builds the named vendor client directly from its own
// *_API_KEY/*_API_SECRET env vars -- never a DB-backed
// provider_credentials lookup (this tool exists precisely for a
// moment when that table may be empty or brokerd may not be running
// at all), and never a hardcoded default credential.
func providerFromFlag(name string) (provider.EnergyProvider, error) {
	switch name {
	case provider.Catfee:
		apiKey := os.Getenv("CATFEE_API_KEY")
		apiSecret := os.Getenv("CATFEE_API_SECRET")
		if apiKey == "" || apiSecret == "" {
			return nil, fmt.Errorf("rent-energy: CATFEE_API_KEY and CATFEE_API_SECRET are both required for -provider catfee")
		}
		return provider.NewCatfeeProvider(provider.CatfeeConfig{APIKey: apiKey, APISecret: apiSecret}), nil
	case provider.Netts:
		apiKey := os.Getenv("NETTS_API_KEY")
		realIP := os.Getenv("NETTS_REAL_IP")
		if apiKey == "" || realIP == "" {
			return nil, fmt.Errorf("rent-energy: NETTS_API_KEY and NETTS_REAL_IP are both required for -provider netts")
		}
		return provider.NewNettsProvider(provider.NettsConfig{APIKey: apiKey, RealIP: realIP}), nil
	case provider.Tronsell:
		apiKey := os.Getenv("TRONSELL_API_KEY")
		baseURL := os.Getenv("TRONSELL_BASE_URL")
		if apiKey == "" || baseURL == "" {
			return nil, fmt.Errorf("rent-energy: TRONSELL_API_KEY and TRONSELL_BASE_URL are both required for -provider tronsell")
		}
		return provider.NewTronsellProvider(provider.TronsellConfig{BaseURL: baseURL, APIKey: apiKey})
	default:
		return nil, fmt.Errorf("rent-energy: unrecognized -provider %q (want catfee, netts, or tronsell)", name)
	}
}
