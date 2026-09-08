// Command dispatchd serves C5, the payout dispatcher, over HTTP.
//
// No background worker is started here: POST /v1/dispatch only performs
// slot selection and EnterDispatching's own E2 conversion entry (see
// internal/httpapi's own package doc comment for why construction,
// signing, broadcast, and finality confirmation are not part of that
// request). Carrying an already-`dispatching` order the rest of the way
// -- and running C5.7's reconciliation job on a ticker -- is a separate
// worker loop this command does not yet start; wiring one in is its own
// task, same as cmd/brokerd's own production loop was built separately
// from C4's HTTP boundary.
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

	"dispatcher/internal/db"
	"dispatcher/internal/dispatch"
	"dispatcher/internal/httpapi"
	"dispatcher/internal/ledgerclient"
	"dispatcher/internal/money"
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

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutdown signal received")
		return httpServer.Shutdown(context.Background())
	}
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
