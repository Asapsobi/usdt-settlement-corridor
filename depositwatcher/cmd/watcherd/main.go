// Command watcherd serves C2, the deposit watcher, over HTTP.
//
// This wires up the HTTP boundary (C2.9) and address derivation (C2.0)
// against a real database. It does NOT yet start the chain-ingestion
// loop or the finality ticker (C2.3/C2.5) -- those need RPC provider
// URLs, a ledgerclient base URL/token, and the USDT BEP20 contract
// address/topic, none of which this codebase has an env-driven config
// step for yet. Server.ChainPool and Server.Tracker are left nil:
// GET /system/providers and the chain-derived fields of
// GET /system/invariants degrade explicitly (see httpapi.Server's own
// doc comment) rather than this command inventing that wiring
// speculatively. Wiring the full engine in is a distinct, later step.
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

	"depositwatcher/internal/addresses"
	"depositwatcher/internal/db"
	"depositwatcher/internal/httpapi"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("watcherd exited with error", "error", err)
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

	xpub := os.Getenv("WATCHER_XPUB")
	if xpub == "" {
		return errors.New("watcherd: WATCHER_XPUB is not set")
	}
	if err := addresses.Configure(xpub); err != nil {
		return err
	}

	auth, err := httpapi.AuthConfigFromEnv()
	if err != nil {
		return err
	}

	router := httpapi.NewRouter(&httpapi.Server{
		Pool:      pool,
		Auth:      auth,
		BuildInfo: buildInfo,
	})

	srv := &http.Server{
		Addr:    listenAddr(),
		Handler: router,
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("watcherd listening", "addr", srv.Addr)
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
	slog.Info("watcherd shut down cleanly")
	return nil
}

func listenAddr() string {
	if addr := os.Getenv("WATCHER_LISTEN_ADDR"); addr != "" {
		return addr
	}
	return ":8082"
}

// buildInfo reads module version and VCS revision from the binary's own
// embedded build metadata rather than requiring -ldflags at build time.
// Same implementation as C1's ledgerd -- duplicated rather than shared
// because these are two separate modules with no common internal package
// between them.
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
