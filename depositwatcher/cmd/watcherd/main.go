// Command watcherd serves C2, the deposit watcher, over HTTP and, when
// WATCHER_RPC_PROVIDERS is set, runs the live chain-watching engine too:
// C2.3's ingestion loop and C2.10's candidate pipeline, reporting to a
// real C1 via ledgerclient. See engine.go's own doc comment for exactly
// which env vars that opts into, and httpapi.Server's for why running
// without it (HTTP boundary only) is a legitimate, supported mode, not a
// half-finished one.
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

	// Built before NewRouter so Server.ChainPool/Tracker are already set
	// by the time NewRouter builds its reactive Prometheus gauges --
	// those read s.ChainPool at scrape time through a closure over the
	// Server pointer, so it must be the real pool by then, not nil.
	eng, err := newEngineFromEnv(pool)
	if err != nil {
		return err
	}

	server := &httpapi.Server{
		Pool:      pool,
		Auth:      auth,
		BuildInfo: buildInfo,
	}
	if eng != nil {
		server.ChainPool = eng.chainPool
		server.Tracker = eng.tracker
	}
	router := httpapi.NewRouter(server)

	if eng != nil {
		engineCtx, stopEngine := context.WithCancel(context.Background())
		defer stopEngine()
		eng.run(engineCtx, pool)
		slog.Info("watcherd: live chain-watching engine started",
			"contract", eng.cfg.ContractAddress, "dust_floor", eng.cfg.DustFloor)
	} else {
		slog.Info("watcherd: WATCHER_RPC_PROVIDERS not set -- serving the HTTP boundary only, no live chain-watching engine")
	}

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
