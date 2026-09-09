// Command proofrund serves docs/03-build/mvp-proof-run-plan.md's own
// minimal order-origination driver over HTTP -- create an order against
// C1, get a deposit address from C2, poll status across C1/C2/C5. See
// internal/driver's own doc comment for what this deliberately is and
// isn't.
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

	"proofrun/internal/driver"
	"proofrun/internal/httpapi"
	"proofrun/internal/money"
	"proofrun/internal/upstream"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("proofrund exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ledgerBaseURL, ledgerToken, err := requireBaseURLAndToken("PROOFRUN_LEDGER_BASE_URL", "PROOFRUN_LEDGER_API_TOKEN")
	if err != nil {
		return err
	}
	watcherBaseURL, watcherToken, err := requireBaseURLAndToken("PROOFRUN_WATCHER_BASE_URL", "PROOFRUN_WATCHER_API_TOKEN")
	if err != nil {
		return err
	}
	dispatcherBaseURL, dispatcherToken, err := requireBaseURLAndToken("PROOFRUN_DISPATCHER_BASE_URL", "PROOFRUN_DISPATCHER_API_TOKEN")
	if err != nil {
		return err
	}

	cfg, err := driverConfigFromEnv()
	if err != nil {
		return err
	}

	d := &driver.Driver{
		Ledger:     upstream.NewLedger(ledgerBaseURL, ledgerToken),
		Watcher:    upstream.NewWatcher(watcherBaseURL, watcherToken),
		Dispatcher: upstream.NewDispatcher(dispatcherBaseURL, dispatcherToken),
		Cfg:        cfg,
	}

	server := &httpapi.Server{Driver: d}
	router := httpapi.NewRouter(server)

	addr := ":8090"
	if v := os.Getenv("PROOFRUN_LISTEN_ADDR"); v != "" {
		addr = v
	}
	httpServer := &http.Server{Addr: addr, Handler: router}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("proofrund listening", "addr", addr)
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

func requireBaseURLAndToken(urlVar, tokenVar string) (string, string, error) {
	url, token := os.Getenv(urlVar), os.Getenv(tokenVar)
	if url == "" || token == "" {
		return "", "", fmt.Errorf("proofrund: %s and %s are both required", urlVar, tokenVar)
	}
	return url, token, nil
}

// driverConfigFromEnv reads PROOFRUN_FEE_BASIS_POINTS, _NETWORK_FEE_UNITS,
// and _QUOTE_VALIDITY -- required, no hardcoded default, same posture
// every other real-money threshold in this project takes, even though
// (unlike those) these three are explicitly placeholder pricing, not a
// production decision (see internal/driver's own doc comment).
func driverConfigFromEnv() (driver.Config, error) {
	feeRaw := os.Getenv("PROOFRUN_FEE_BASIS_POINTS")
	if feeRaw == "" {
		return driver.Config{}, fmt.Errorf("proofrund: PROOFRUN_FEE_BASIS_POINTS is required (25 = the 25bp fee product-operations-architecture.md's own pricing analysis uses)")
	}
	feeBps, err := strconv.ParseInt(feeRaw, 10, 64)
	if err != nil {
		return driver.Config{}, fmt.Errorf("proofrund: PROOFRUN_FEE_BASIS_POINTS: %w", err)
	}

	networkFeeRaw := os.Getenv("PROOFRUN_NETWORK_FEE_UNITS")
	if networkFeeRaw == "" {
		return driver.Config{}, fmt.Errorf("proofrund: PROOFRUN_NETWORK_FEE_UNITS is required (a flat placeholder, decimal string, e.g. \"1.800000\")")
	}
	networkFeeUnits, err := money.ParseDecimal(networkFeeRaw)
	if err != nil {
		return driver.Config{}, fmt.Errorf("proofrund: PROOFRUN_NETWORK_FEE_UNITS: %w", err)
	}

	quoteValidityRaw := os.Getenv("PROOFRUN_QUOTE_VALIDITY")
	if quoteValidityRaw == "" {
		return driver.Config{}, fmt.Errorf("proofrund: PROOFRUN_QUOTE_VALIDITY is required (e.g. \"24h\" -- generous on purpose, see internal/driver's own Config doc comment)")
	}
	quoteValidity, err := time.ParseDuration(quoteValidityRaw)
	if err != nil {
		return driver.Config{}, fmt.Errorf("proofrund: PROOFRUN_QUOTE_VALIDITY: %w", err)
	}

	return driver.Config{FeeBasisPoints: feeBps, NetworkFeeUnits: networkFeeUnits, QuoteValidity: quoteValidity}, nil
}
