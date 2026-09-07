// Command brokerd serves C4, the energy broker, over HTTP: bearer auth,
// /healthz, /readyz, /metrics, and the full business API under /v1
// (reservations, buffer visibility, manual-fallback events, system
// prices/invariants) backed by real Tronsell/Netts/CatFee vendor
// integrations.
//
// JustLendManual is always wired to provider.NoOpProvider{} -- see that
// type's own doc comment, and "Read this second" in
// docs/03-build/c4-energy-broker-build-prompts.md: automating the
// on-chain-signing path JustLendDAO would require is out of scope, by
// design, at MVP. A vendor outage spanning all three primaries falls
// through to the manual runbook (docs/runbook-energy-fallback.md), not
// an automated fourth provider.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"energybroker/internal/buffer"
	"energybroker/internal/db"
	"energybroker/internal/httpapi"
	"energybroker/internal/ledgerclient"
	"energybroker/internal/pricing"
	"energybroker/internal/provider"
	"energybroker/internal/reservations"
	"energybroker/internal/routing"
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

	providers, err := providersFromEnv()
	if err != nil {
		return err
	}

	slotAddresses, err := slotAddressesFromEnv()
	if err != nil {
		return err
	}

	weights, err := routingWeightsFromEnv()
	if err != nil {
		return err
	}

	ceiling, err := ceilingFromEnv()
	if err != nil {
		return err
	}

	ledgerBaseURL := os.Getenv("LEDGER_BASE_URL")
	ledgerToken := os.Getenv("LEDGER_API_TOKEN")
	if ledgerBaseURL == "" || ledgerToken == "" {
		return errors.New("brokerd: LEDGER_BASE_URL and LEDGER_API_TOKEN must both be set")
	}
	ledger := ledgerclient.New(ledgerBaseURL, ledgerToken)

	poller := pricing.NewPoller(pool, providers, 0) // 0 -> pricing.DefaultStaleness
	router := routing.NewRouter(poller, pool, rand.New(rand.NewSource(time.Now().UnixNano())).Int63())

	tronReader := tronEnergyReaderFromEnv()

	// reservations.DemandObserver{Pool: pool} sidesteps a genuine
	// circular constructor dependency: buffer.NewBuffer needs a
	// DemandObserver, but the natural one (the reservations table) is
	// otherwise only reachable through a *reservations.Service, which
	// itself needs this *buffer.Buffer to construct -- see
	// DemandObserver's own doc comment.
	buf, err := buffer.NewBuffer(pool, providers, router, reservations.DemandObserver{Pool: pool}, tronReader, nil, buffer.Config{
		Weights:       weights,
		Ceiling:       ceiling,
		SlotAddresses: slotAddresses,
	})
	if err != nil {
		return fmt.Errorf("brokerd: %w", err)
	}

	reservationSvc, err := reservations.NewService(pool, ledger, buf, router, ledger, nil, providers, reservations.Config{
		Weights: weights,
		Ceiling: ceiling,
	})
	if err != nil {
		return fmt.Errorf("brokerd: %w", err)
	}

	server := &httpapi.Server{
		Pool:         pool,
		Auth:         auth,
		Reservations: reservationSvc,
		Buffer:       buf,
		Router:       router,
		Poller:       poller,
		Weights:      weights,
		Ceiling:      ceiling,
		BuildInfo:    buildInfo,
	}
	httpRouter := httpapi.NewRouter(server)

	go func() {
		if err := poller.RunLoop(ctx, 0); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("brokerd: price poller loop exited", "error", err)
		}
	}()
	go func() {
		if err := buf.RunLoop(ctx, 0); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("brokerd: buffer replenish loop exited", "error", err)
		}
	}()
	go func() {
		if err := buf.RunReconcileLoop(ctx, 0); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("brokerd: buffer reconcile loop exited", "error", err)
		}
	}()

	srv := &http.Server{
		Addr:    listenAddr(),
		Handler: httpRouter,
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

// providersFromEnv builds the four named provider slots (see
// internal/provider's own Provider name constants): real HTTP clients
// for the three primaries, and provider.NoOpProvider{} for
// justlend_manual, always -- see this file's own doc comment.
func providersFromEnv() (map[string]provider.EnergyProvider, error) {
	tronsellBaseURL := os.Getenv("BROKER_TRONSELL_BASE_URL")
	if tronsellBaseURL == "" {
		return nil, fmt.Errorf("brokerd: BROKER_TRONSELL_BASE_URL is not set -- %w", provider.ErrTronsellBaseURLNotConfigured)
	}
	tronsell, err := provider.NewTronsellProvider(provider.TronsellConfig{
		BaseURL: tronsellBaseURL,
		APIKey:  os.Getenv("BROKER_TRONSELL_API_KEY"),
	})
	if err != nil {
		return nil, fmt.Errorf("brokerd: %w", err)
	}

	nettsRealIP := os.Getenv("BROKER_NETTS_REAL_IP")
	if nettsRealIP == "" {
		return nil, errors.New("brokerd: BROKER_NETTS_REAL_IP is not set -- Netts requires this deployment's own whitelisted egress IP")
	}
	netts := provider.NewNettsProvider(provider.NettsConfig{
		APIKey: os.Getenv("BROKER_NETTS_API_KEY"),
		RealIP: nettsRealIP,
	})

	catfee := provider.NewCatfeeProvider(provider.CatfeeConfig{
		APIKey:    os.Getenv("BROKER_CATFEE_API_KEY"),
		APISecret: os.Getenv("BROKER_CATFEE_API_SECRET"),
	})

	return map[string]provider.EnergyProvider{
		provider.Tronsell:       tronsell,
		provider.Netts:          netts,
		provider.Catfee:         catfee,
		provider.JustLendManual: provider.NoOpProvider{},
	}, nil
}

// slotAddressesFromEnv reads BROKER_PAYOUT_SLOT_ADDRESSES, a
// comma-separated list of the real payout slot addresses this buffer
// pre-provisions against (component-map's own "six slots capped at
// $50k/5,000 tx each") -- required, never fabricated here: only whoever
// owns the payout wallet roster (S1) knows the real values.
func slotAddressesFromEnv() ([]string, error) {
	raw := os.Getenv("BROKER_PAYOUT_SLOT_ADDRESSES")
	if raw == "" {
		return nil, errors.New("brokerd: BROKER_PAYOUT_SLOT_ADDRESSES is not set")
	}
	var addresses []string
	for _, addr := range strings.Split(raw, ",") {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		addresses = append(addresses, addr)
	}
	if len(addresses) == 0 {
		return nil, errors.New("brokerd: BROKER_PAYOUT_SLOT_ADDRESSES contained no usable addresses")
	}
	return addresses, nil
}

// routingWeightsFromEnv reads BROKER_ROUTING_WEIGHTS, formatted as
// "tronsell:0.60,netts:0.35,catfee:0.05" -- the same "name:value,..."
// shape httpapi.AuthConfigFromEnv already uses for BROKER_API_TOKENS.
func routingWeightsFromEnv() (routing.RoutingWeights, error) {
	raw := os.Getenv("BROKER_ROUTING_WEIGHTS")
	if raw == "" {
		return nil, errors.New("brokerd: BROKER_ROUTING_WEIGHTS is not set")
	}
	weights := make(routing.RoutingWeights)
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, value, ok := strings.Cut(pair, ":")
		if !ok || name == "" || value == "" {
			return nil, fmt.Errorf("brokerd: malformed BROKER_ROUTING_WEIGHTS entry %q, want name:weight", pair)
		}
		weight, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, fmt.Errorf("brokerd: BROKER_ROUTING_WEIGHTS entry %q: %w", pair, err)
		}
		weights[name] = weight
	}
	if len(weights) == 0 {
		return nil, errors.New("brokerd: BROKER_ROUTING_WEIGHTS contained no usable name:weight pairs")
	}
	return weights, nil
}

// tronEnergyReaderFromEnv builds the real, TronGrid-backed on-chain
// reader invariant 1 depends on for every VerifyOnChain/Reconcile call
// this binary makes -- BROKER_TRONGRID_API_KEY is optional (TronGrid's
// own basic account-resource query needs no auth) but recommended, so
// this deployment gets its own rate-limit allowance rather than sharing
// TronGrid's public, unauthenticated one.
func tronEnergyReaderFromEnv() *buffer.TronGridReader {
	return buffer.NewTronGridReader(buffer.TronGridConfig{
		BaseURL: os.Getenv("BROKER_TRONGRID_BASE_URL"),
		APIKey:  os.Getenv("BROKER_TRONGRID_API_KEY"),
	})
}

// ceilingFromEnv reads BROKER_PRICE_CEILING_SUN -- the per-unit sun
// price never paid through, under any circumstance (invariant 3).
func ceilingFromEnv() (float64, error) {
	raw := os.Getenv("BROKER_PRICE_CEILING_SUN")
	if raw == "" {
		return 0, errors.New("brokerd: BROKER_PRICE_CEILING_SUN is not set")
	}
	ceiling, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("brokerd: BROKER_PRICE_CEILING_SUN: %w", err)
	}
	if ceiling <= 0 {
		return 0, fmt.Errorf("brokerd: BROKER_PRICE_CEILING_SUN must be positive, got %v", ceiling)
	}
	return ceiling, nil
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
