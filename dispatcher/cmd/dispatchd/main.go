// Command dispatchd serves C5, the payout dispatcher, over HTTP, and
// (unlike previously) runs internal/orchestrate's own background loop:
// carrying a screened Direct/Standard order from EnterDispatching
// through energy reservation, signing, broadcast, and finality
// confirmation with no manual step. POST /v1/dispatch (internal/httpapi)
// still exists separately for an operator-triggered dispatch, but
// nothing requires calling it now -- the loop below picks up every
// screened order on its own.
//
// C5.7's reconciliation job (internal/dispatch.ReconcileFailedDispatches)
// is still not started here -- a real DispatchFailureReporter and its
// own polling cadence is a separate, not-yet-scoped task, same posture
// this file previously took toward the whole background loop.
//
// Sweep-tier batches (CutBatch/BroadcastBatch) are also not driven by
// any loop here -- see internal/orchestrate's own package doc comment
// for why (no real multisend contract exists on either chain yet).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"dispatcher/internal/db"
	"dispatcher/internal/dispatch"
	"dispatcher/internal/energy"
	"dispatcher/internal/httpapi"
	"dispatcher/internal/ledgerclient"
	"dispatcher/internal/money"
	"dispatcher/internal/orchestrate"
	"dispatcher/internal/signing"
	"dispatcher/internal/slots"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("dispatchd exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dbCfg, err := db.ConfigFromEnv()
	if err != nil {
		return err
	}
	pool, err := db.Open(ctx, dbCfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	auth, err := httpapi.AuthConfigFromEnv()
	if err != nil {
		return err
	}

	ledgerBaseURL := os.Getenv("DISPATCHER_LEDGER_BASE_URL")
	ledgerToken := os.Getenv("DISPATCHER_LEDGER_API_TOKEN")
	if ledgerBaseURL == "" || ledgerToken == "" {
		return fmt.Errorf("dispatchd: DISPATCHER_LEDGER_BASE_URL and DISPATCHER_LEDGER_API_TOKEN are required")
	}
	ledgerClient := ledgerclient.New(ledgerBaseURL, ledgerToken)

	signingBaseURL := os.Getenv("DISPATCHER_SIGNING_BASE_URL")
	signingToken := os.Getenv("DISPATCHER_SIGNING_API_TOKEN")
	if signingBaseURL == "" || signingToken == "" {
		return fmt.Errorf("dispatchd: DISPATCHER_SIGNING_BASE_URL and DISPATCHER_SIGNING_API_TOKEN are required")
	}
	signingClient := signing.New(signingBaseURL, signingToken)

	slotCaps, err := slotCapsFromEnv()
	if err != nil {
		return err
	}

	slotsStore := slots.NewStore(pool)
	dispatcher := dispatch.NewDispatcher(ledgerClient, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signingClient)
	dispatcher.Batches = dispatch.NewBatchStore(pool)

	server := &httpapi.Server{
		Pool: pool, Auth: auth, Dispatcher: dispatcher, Slots: slotsStore,
		Ledger: ledgerClient, SlotCaps: slotCaps,
		BuildInfo: func() (string, string) { return "dev", "dev" },
	}
	router := httpapi.NewRouter(server)

	orchestrator, orchestrateInterval, err := orchestratorFromEnv(ledgerClient, slotsStore, slotCaps, dispatcher)
	if err != nil {
		return err
	}

	addr := ":8086"
	if v := os.Getenv("DISPATCHER_LISTEN_ADDR"); v != "" {
		addr = v
	}
	httpServer := &http.Server{Addr: addr, Handler: router}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("dispatchd listening", "addr", addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	go func() {
		slog.Info("orchestrate loop starting", "interval", orchestrateInterval)
		if err := orchestrator.RunLoop(ctx, orchestrateInterval); err != nil && err != context.Canceled {
			errCh <- fmt.Errorf("dispatchd: orchestrate loop: %w", err)
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutdown signal received")
		return httpServer.Shutdown(context.Background())
	}
}

// orchestratorFromEnv wires internal/orchestrate against a real energy
// broker (C4) and real TRON node -- DISPATCHER_ENERGY_BASE_URL/
// _API_TOKEN (matching DISPATCHER_LEDGER_/_SIGNING_'s own naming),
// DISPATCHER_TRON_GRPC_ADDR (e.g. "grpc.trongrid.io:50051", broadcast
// and current-block-reference resolution) and DISPATCHER_TRON_API_BASE_URL
// (e.g. "https://api.trongrid.io", finality reads) -- both real,
// previously unwired dispatch.GrpcBroadcastClient/HTTPFinalityReader
// (internal/dispatch/tronchain.go). DISPATCHER_ENERGY_PER_TRANSFER_UNITS
// and DISPATCHER_ORCHESTRATE_INTERVAL are required, no hardcoded
// default, same posture as every other real-money threshold in this
// project.
func orchestratorFromEnv(ledgerClient *ledgerclient.Client, slotsStore *slots.Store, slotCaps slots.Caps, dispatcher *dispatch.Dispatcher) (*orchestrate.Orchestrator, time.Duration, error) {
	energyBaseURL := os.Getenv("DISPATCHER_ENERGY_BASE_URL")
	energyToken := os.Getenv("DISPATCHER_ENERGY_API_TOKEN")
	if energyBaseURL == "" || energyToken == "" {
		return nil, 0, fmt.Errorf("dispatchd: DISPATCHER_ENERGY_BASE_URL and DISPATCHER_ENERGY_API_TOKEN are required")
	}
	energyClient := energy.New(energyBaseURL, energyToken)

	tronGRPCAddr := os.Getenv("DISPATCHER_TRON_GRPC_ADDR")
	if tronGRPCAddr == "" {
		return nil, 0, fmt.Errorf("dispatchd: DISPATCHER_TRON_GRPC_ADDR is required")
	}
	chain, err := dispatch.NewGrpcBroadcastClient(tronGRPCAddr, 15*time.Second)
	if err != nil {
		return nil, 0, fmt.Errorf("dispatchd: connecting to TRON node %q: %w", tronGRPCAddr, err)
	}

	tronAPIBaseURL := os.Getenv("DISPATCHER_TRON_API_BASE_URL")
	if tronAPIBaseURL == "" {
		return nil, 0, fmt.Errorf("dispatchd: DISPATCHER_TRON_API_BASE_URL is required")
	}
	finality := dispatch.NewHTTPFinalityReader(tronAPIBaseURL)

	energyUnitsRaw := os.Getenv("DISPATCHER_ENERGY_PER_TRANSFER_UNITS")
	if energyUnitsRaw == "" {
		return nil, 0, fmt.Errorf("dispatchd: DISPATCHER_ENERGY_PER_TRANSFER_UNITS is required")
	}
	energyUnits, err := strconv.ParseInt(energyUnitsRaw, 10, 64)
	if err != nil {
		return nil, 0, fmt.Errorf("dispatchd: DISPATCHER_ENERGY_PER_TRANSFER_UNITS: %w", err)
	}

	interval := orchestrate.DefaultInterval
	if v := os.Getenv("DISPATCHER_ORCHESTRATE_INTERVAL"); v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return nil, 0, fmt.Errorf("dispatchd: DISPATCHER_ORCHESTRATE_INTERVAL: %w", err)
		}
		interval = parsed
	}

	cfg := orchestrate.Config{EnergyPerTransferUnits: energyUnits, EnergyReservationWait: time.Minute}
	return orchestrate.New(ledgerClient, slotsStore, slotCaps, dispatcher, energyClient, chain, chain, finality, cfg), interval, nil
}

// slotCapsFromEnv reads DISPATCHER_SLOT_BALANCE_CEILING (decimal string,
// USDT) and DISPATCHER_SLOT_TX_COUNT_CEILING (integer) -- decision 4's
// own $50k / 5,000-tx per-slot caps, required config, no hardcoded
// default (the same posture every other real-money threshold in this
// project takes).
func slotCapsFromEnv() (slots.Caps, error) {
	balanceRaw := os.Getenv("DISPATCHER_SLOT_BALANCE_CEILING")
	if balanceRaw == "" {
		return slots.Caps{}, fmt.Errorf("dispatchd: DISPATCHER_SLOT_BALANCE_CEILING is required")
	}
	balance, err := money.ParseDecimal(balanceRaw)
	if err != nil {
		return slots.Caps{}, fmt.Errorf("dispatchd: DISPATCHER_SLOT_BALANCE_CEILING: %w", err)
	}

	txCountRaw := os.Getenv("DISPATCHER_SLOT_TX_COUNT_CEILING")
	if txCountRaw == "" {
		return slots.Caps{}, fmt.Errorf("dispatchd: DISPATCHER_SLOT_TX_COUNT_CEILING is required")
	}
	txCount, err := strconv.ParseInt(txCountRaw, 10, 64)
	if err != nil {
		return slots.Caps{}, fmt.Errorf("dispatchd: DISPATCHER_SLOT_TX_COUNT_CEILING: %w", err)
	}

	return slots.Caps{BalanceCeiling: balance, TxCountCeiling: txCount}, nil
}
