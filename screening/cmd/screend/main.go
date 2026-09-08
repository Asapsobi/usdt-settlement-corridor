// Command screend serves C3, the screening service, over HTTP: the
// hold queue, screening-result audit lookups, and re-screen flags
// (C3.8) -- plus, now, the discovery background loop (C3.3), which
// polls C1 for newly-funded orders and resolves their sender_address,
// both against a real ledgerclient.Client (no fake, no vendor
// dependency).
//
// It does NOT run the pipeline or re-screen background loops
// (C3.4/C3.7's own engines). Both need a real provider.ScreeningProvider
// -- a real AML vendor client, one of the candidates component-map.md's
// own C3 "provider call" line names without picking one -- and
// internal/provider only has MockProvider. The C3 build doc is explicit
// that vendor choice was deliberately left unmade ("vendor abstraction,
// not vendor choice"); wiring a mock into a production compliance path
// would silently screen real orders against a fake, seeded verdict,
// which is worse than the honest gap left here. A deployment running
// this binary today discovers funded orders and serves the manual-
// review/audit surface correctly; it needs a real ScreeningProvider
// added, and pipeline.RunLoop/rescreen.RunLoop wired against it, before
// it screens anything on its own.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"screening/internal/db"
	"screening/internal/discovery"
	"screening/internal/httpapi"
	"screening/internal/ledgerclient"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("screend exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := db.ConfigFromEnv()
	if err != nil {
		return err
	}

	pool, err := db.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	auth, err := httpapi.AuthConfigFromEnv()
	if err != nil {
		return err
	}

	ledgerBaseURL := os.Getenv("SCREENING_LEDGER_BASE_URL")
	ledgerToken := os.Getenv("SCREENING_LEDGER_TOKEN")
	if ledgerBaseURL == "" || ledgerToken == "" {
		return errors.New("screend: SCREENING_LEDGER_BASE_URL and SCREENING_LEDGER_TOKEN are both required " +
			"(POST /holds/{id}/release and /reject, and the discovery loop's own PollFundedOrders/GetSenderAddress, all call C1 through this client)")
	}
	ledgerClient := ledgerclient.New(ledgerBaseURL, ledgerToken)

	server := &httpapi.Server{
		Pool:         pool,
		Auth:         auth,
		LedgerClient: ledgerClient,
		BuildInfo:    buildInfo,
	}
	router := httpapi.NewRouter(server)

	// ledgerClient implements both discovery.FundedOrderPoller
	// (PollFundedOrders) and provider.SenderAddressLookup
	// (GetSenderAddress) for real, against the real, running C1 this
	// binary was configured to talk to above -- no fake, no vendor
	// dependency, so this loop has nothing blocking it from running in
	// production today. See this file's own doc comment for why
	// pipeline/rescreen aren't started alongside it.
	go func() {
		if err := discovery.RunLoop(ctx, pool, ledgerClient, ledgerClient, 0); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("screend: discovery loop exited", "error", err)
		}
	}()

	srv := &http.Server{
		Addr:    listenAddr(),
		Handler: router,
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("screend listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	case err := <-serveErr:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	slog.Info("screend shut down cleanly")
	return nil
}

func listenAddr() string {
	if addr := os.Getenv("SCREENING_LISTEN_ADDR"); addr != "" {
		return addr
	}
	return ":8083"
}

// buildInfo reads module version and VCS revision from the binary's own
// embedded build metadata. Same implementation as C1's ledgerd and C2's
// watcherd -- duplicated rather than shared, since these are three
// separate modules with no common internal package between them.
func buildInfo() (version, commit string) {
	version, commit = "unknown", "unknown"
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if info.Main.Version != "" {
		version = info.Main.Version
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			commit = s.Value
		}
	}
	return
}
