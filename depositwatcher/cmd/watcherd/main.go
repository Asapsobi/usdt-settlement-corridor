// Command watcherd serves C2, the deposit watcher. This chunk (C2.0) only
// proves the scaffold: it connects to its own database and serves a
// health check. No chain access, no address assignment, no HTTP boundary
// beyond /healthz -- those are later chunks.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"depositwatcher/internal/db"
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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthzHandler(pool))

	srv := &http.Server{
		Addr:    listenAddr(),
		Handler: mux,
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

type healthzResponse struct {
	Status string `json:"status"`
	DB     string `json:"db"`
}

// healthzHandler pings the database on every call rather than only at
// startup -- a connection that was fine at boot and dies later (the
// database restarts, a network partition) must be visible here, not
// masked by a check that only ever ran once.
func healthzHandler(pool *db.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := healthzResponse{Status: "ok", DB: "ok"}
		status := http.StatusOK
		if err := pool.Ping(r.Context()); err != nil {
			resp.Status = "degraded"
			resp.DB = "unreachable"
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func listenAddr() string {
	if addr := os.Getenv("WATCHER_LISTEN_ADDR"); addr != "" {
		return addr
	}
	return ":8082"
}
