// Command reconcile-reservation confirms a reservation the slow path
// marked FAILED, given a delegation the operator has independently
// verified is real (e.g. by reading the vendor's own target address
// directly on-chain) -- see reservations.Service.ManualConfirm's own
// doc comment for the exact gap this closes. Never called from any
// automatic path; the operator vouches for the delegation being real.
//
// Usage:
//
//	ENERGY_DATABASE_URL=... LEDGER_BASE_URL=... LEDGER_API_TOKEN=... \
//	  reconcile-reservation -reservation-id 1 -order-id 1 \
//	    -provider catfee -delegation-id manual-2026-09-10-1 \
//	    -target-address THsJ1H5... -energy-units 64999 -cost-trx 1.300000
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"energybroker/internal/db"
	"energybroker/internal/ledgerclient"
	"energybroker/internal/money"
	"energybroker/internal/provider"
	"energybroker/internal/reservations"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	reservationID := flag.Int64("reservation-id", 0, "the FAILED reservation's own id (required)")
	orderID := flag.Int64("order-id", 0, "C1's own order id this reservation was for (required)")
	providerName := flag.String("provider", "", "vendor name, e.g. catfee (required)")
	delegationID := flag.String("delegation-id", "", "a unique id for this delegation -- used as the ledger entry's own idempotency key (required)")
	targetAddress := flag.String("target-address", "", "the TRON address the energy was delegated to (required)")
	energyUnits := flag.Int64("energy-units", 0, "energy units independently confirmed on-chain (required, > 0)")
	costTRXRaw := flag.String("cost-trx", "", "the real cost, decimal TRX string, e.g. 1.300000 (required)")
	flag.Parse()

	if *reservationID == 0 || *orderID == 0 || *providerName == "" || *delegationID == "" || *targetAddress == "" || *energyUnits <= 0 || *costTRXRaw == "" {
		return fmt.Errorf("reconcile-reservation: -reservation-id, -order-id, -provider, -delegation-id, -target-address, -energy-units, and -cost-trx are all required")
	}
	costTRX, err := money.ParseDecimal(*costTRXRaw)
	if err != nil {
		return fmt.Errorf("reconcile-reservation: -cost-trx: %w", err)
	}

	ctx := context.Background()
	dbCfg, err := db.ConfigFromEnv()
	if err != nil {
		return err
	}
	pool, err := db.Open(ctx, dbCfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	ledgerBaseURL := os.Getenv("LEDGER_BASE_URL")
	ledgerToken := os.Getenv("LEDGER_API_TOKEN")
	if ledgerBaseURL == "" || ledgerToken == "" {
		return fmt.Errorf("reconcile-reservation: LEDGER_BASE_URL and LEDGER_API_TOKEN are required")
	}
	ledger := ledgerclient.New(ledgerBaseURL, ledgerToken)

	svc, err := reservations.NewService(pool, nil, nil, nil, ledger, nil, nil, reservations.Config{Ceiling: 1})
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	delegation := provider.Delegation{
		ID: *delegationID, ProviderName: *providerName, TargetAddress: *targetAddress,
		EnergyUnits: *energyUnits, CostTRX: costTRX, RequestedAt: now, ExpiresAt: now.Add(time.Hour), ConfirmedAt: &now,
	}

	r, err := svc.ManualConfirm(ctx, *reservationID, *orderID, delegation)
	if err != nil {
		return fmt.Errorf("reconcile-reservation: %w", err)
	}
	fmt.Printf("ok   reservation %d confirmed, vendor=%s cost_trx=%s status=%s\n", r.ID, *r.Vendor, r.CostTRX.Format(), r.Status)
	return nil
}
