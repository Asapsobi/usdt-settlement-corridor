// Command screend serves C3, the screening service, over HTTP: the
// hold queue, screening-result audit lookups, and re-screen flags
// (C3.8). It does NOT yet run the automatic pipeline, discovery, or
// re-screen background loops (C3.4/C3.3/C3.7's own engines) -- that
// wiring is deliberately out of this chunk's scope, the same way
// depositwatcher's own watcherd engine wiring landed as a separate
// effort from any single numbered C2 chunk. A deployment running this
// binary today serves the manual-review and audit surface correctly;
// it needs the engine wiring added separately before it screens
// anything on its own.
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
			"(POST /holds/{id}/release and /reject call C1 through this client)")
	}
	ledgerClient := ledgerclient.New(ledgerBaseURL, ledgerToken)

	server := &httpapi.Server{
		Pool:         pool,
		Auth:         auth,
		LedgerClient: ledgerClient,
		BuildInfo:    buildInfo,
	}
	router := httpapi.NewRouter(server)

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
