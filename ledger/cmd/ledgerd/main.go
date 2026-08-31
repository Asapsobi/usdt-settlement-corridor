// Command ledgerd serves C1, the ledger core, over HTTP.
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

	"ledger/internal/db"
	"ledger/internal/halt"
	"ledger/internal/httpapi"
	"ledger/internal/orders"
	"ledger/internal/recon"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("ledgerd exited with error", "error", err)
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

	orders.SetHaltCache(halt.NewCache(pool.Pool))

	reconCfg, err := recon.ConfigFromEnv()
	if err != nil {
		return err
	}
	reconciler := recon.NewReconciler(pool.Pool, reconCfg)
	go reconciler.Run(ctx)
	slog.Info("reconciler self-check loop started", "interval", reconCfg.Interval)

	auth, err := httpapi.AuthConfigFromEnv()
	if err != nil {
		return err
	}

	router := httpapi.NewRouter(&httpapi.Server{
		Pool:       pool,
		Auth:       auth,
		ReconCfg:   reconCfg,
		Reconciler: reconciler,
		BuildInfo:  buildInfo,
	})

	srv := &http.Server{
		Addr:    listenAddr(),
		Handler: router,
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("ledgerd listening", "addr", srv.Addr)
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
	slog.Info("ledgerd shut down cleanly")
	return nil
}

func listenAddr() string {
	if addr := os.Getenv("LEDGER_LISTEN_ADDR"); addr != "" {
		return addr
	}
	return ":8080"
}

// buildInfo reads module version and VCS revision from the binary's own
// embedded build metadata rather than requiring -ldflags at build time.
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
