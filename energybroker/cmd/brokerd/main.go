// Command brokerd serves C4, the energy broker, over HTTP: bearer auth
// plus /healthz, /readyz, and /metrics (C4.8's own HTTP boundary).
//
// It does NOT wire Reservations, Buffer, Router, or Poller -- the same
// posture screening's own screend takes toward its background engines
// (see that binary's own doc comment): those four all need a real
// EnergyProvider per primary vendor (Tronsell/Netts/Catfee), and no
// chunk in this project has built one yet -- internal/provider only
// exports MockProvider and NoOpProvider, both test/scaffolding-only.
// Wiring fakes into a "production" binary would be worse than leaving
// the gap explicit: a deployment running this binary today correctly
// serves the operational surface (auth, health, metrics) but every
// business endpoint under /v1 (POST /reservations, GET /buffer,
// manual-fallback-events, system/prices, system/invariants) panics on a
// nil dependency (recovered per-request by net/http, not a clean error
// response) until a real vendor integration lands and this file is
// updated alongside it.
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

	"energybroker/internal/db"
	"energybroker/internal/httpapi"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("brokerd exited with error", "error", err)
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

	server := &httpapi.Server{
		Pool:      pool,
		Auth:      auth,
		BuildInfo: buildInfo,
	}
	router := httpapi.NewRouter(server)

	srv := &http.Server{
		Addr:    listenAddr(),
		Handler: router,
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("brokerd listening", "addr", srv.Addr)
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
	slog.Info("brokerd shut down cleanly")
	return nil
}

func listenAddr() string {
	if addr := os.Getenv("BROKER_LISTEN_ADDR"); addr != "" {
		return addr
	}
	return ":8084"
}

// buildInfo reads module version and VCS revision from the binary's own
// embedded build metadata. Same implementation as every prior
// component's own daemon -- duplicated rather than shared, since these
// are separate modules with no common internal package between them.
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
