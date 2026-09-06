// Package cache is C3's result cache by sender address: it stores every
// raw provider Verdict, keyed by (provider, address), and answers "is
// there a still-trusted one" without ever overwriting or deleting a past
// result -- the same append-only audit instinct C1's journal uses, for
// the same reason (the scenario catalog's own worry is that a wrong
// cached verdict can propagate across multiple orders; that is only
// debuggable if every verdict this cache ever produced is still there).
//
// This package does not classify a Verdict into pass/hold/reject -- that
// is internal/verdict's job (C3.2). It only answers "what did we last
// see, and is it still good enough to reuse."
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"screening/internal/db"
	"screening/internal/provider"
)

// Queryer is db.Queryer under this package's own name, matching every
// other internal package's convention in this module.
type Queryer = db.Queryer

// TTLConfig is how long a cached verdict stays trusted, split by outcome
// -- a flagged result and a clean result do not need the same lifetime.
// These are placeholders pending a real product/compliance decision (see
// docs/03-build/c3-screening-build-prompts.md's C3.1 section) -- ship
// loud about that, not silently baked in as if considered settled.
type TTLConfig struct {
	// Clean is how long a non-flagged verdict is trusted before a fresh
	// check is required.
	Clean time.Duration
	// Flagged is how long a flagged verdict is trusted. Longer than
	// Clean by default: a flagged sender doesn't stop being a risk just
	// because time passed, whereas a clean sender's risk can change and
	// deserves more frequent re-checking.
	Flagged time.Duration
}

// DefaultTTLConfig is a conservative placeholder -- 24h clean, 7d
// flagged -- not a real policy decision. See TTLConfig's own doc comment.
var DefaultTTLConfig = TTLConfig{
	Clean:   24 * time.Hour,
	Flagged: 7 * 24 * time.Hour,
}

// For returns the TTL that applies to v under cfg.
func (cfg TTLConfig) For(v provider.Verdict) time.Duration {
	if v.Flagged {
		return cfg.Flagged
	}
	return cfg.Clean
}

// Hit is a cache lookup result: the persisted Verdict plus the
// screening_results row id it came from. The id matters beyond bare
// lookup: invariant 3's idempotency-key format
// ("screening:<verdict>:<order_id>:<screening_result_id>") ties a
// reported transition to WHICH screening result produced it, not just
// which order -- so a legitimate re-screen after Invalidate (a new row,
// a new id) produces a genuinely different idempotency key rather than
// deduping against a stale prior report. internal/ledgerclient's
// ReportVerdict (C3.4) is the consumer that needs this.
type Hit struct {
	provider.Verdict
	ID int64
}

// Get returns the freshest cached result for (providerName, address)
// that is both unexpired and not covered by a later Invalidate call, or
// nil if there is none. It never returns an expired or invalidated row --
// callers that need history for audit purposes query screening_results
// directly, this is the "is there something to reuse right now" answer
// only.
func Get(ctx context.Context, q Queryer, providerName, address string) (*Hit, error) {
	invalidatedAt, err := latestInvalidation(ctx, q, providerName, address)
	if err != nil {
		return nil, err
	}

	row := q.QueryRow(ctx, `
		SELECT id, risk_score, flagged, reason_codes, raw_response, checked_at
		FROM screening_results
		WHERE provider_name = $1
		  AND sender_address = $2
		  AND expires_at > now()
		  AND ($3::timestamptz IS NULL OR checked_at > $3)
		ORDER BY checked_at DESC
		LIMIT 1
	`, providerName, address, invalidatedAt)

	var hit Hit
	var raw []byte
	if err := row.Scan(&hit.ID, &hit.RiskScore, &hit.Flagged, &hit.ReasonCodes, &raw, &hit.CheckedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cache: get: %w", err)
	}
	hit.RawResponse = json.RawMessage(raw)
	hit.ProviderName = providerName

	// "For which order" (the scenario catalog's own concern about a
	// stale verdict propagating silently) is logged by the caller, which
	// is the only layer that knows the order this Get was made on behalf
	// of -- see internal/verdict/C3.4. This layer logs everything it
	// itself knows: which cache key was reused, and from when.
	slog.Info("cache: hit", "provider", providerName, "sender_address", address, "screening_result_id", hit.ID, "checked_at", hit.CheckedAt)
	return &hit, nil
}

// LatestAny returns the single most recent screening_results row for
// (providerName, address) regardless of expiry or invalidation -- nil if
// none exists at all. Unlike Get, this is not "is there something
// trustworthy to reuse right now"; it answers "what was the last thing
// this pair actually saw", which is what internal/rescreen (C3.7) needs
// to compare a fresh re-check against: the verdict that let an order
// through can legitimately have expired from Get's own trust window by
// the time a re-screen runs (TTLs are hours to days; an order can sit in
// screened/dispatching for a while), but it's still the exact fact a
// re-screen needs to diff against, not a "no data" result.
func LatestAny(ctx context.Context, q Queryer, providerName, address string) (*Hit, error) {
	row := q.QueryRow(ctx, `
		SELECT id, risk_score, flagged, reason_codes, raw_response, checked_at
		FROM screening_results
		WHERE provider_name = $1 AND sender_address = $2
		ORDER BY checked_at DESC
		LIMIT 1
	`, providerName, address)

	var hit Hit
	var raw []byte
	if err := row.Scan(&hit.ID, &hit.RiskScore, &hit.Flagged, &hit.ReasonCodes, &raw, &hit.CheckedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cache: latest any: %w", err)
	}
	hit.RawResponse = json.RawMessage(raw)
	hit.ProviderName = providerName
	return &hit, nil
}

// Put records v as a new screening_results row for (providerName,
// address), never overwriting any prior row for the same key -- a fresh
// check is always a new insert, preserving full history. expires_at is
// v.CheckedAt (or, if that's zero, now) plus ttl. Returns the new row's
// id, for the same reason Get's Hit carries one.
func Put(ctx context.Context, q Queryer, providerName, address string, v provider.Verdict, ttl time.Duration) (id int64, err error) {
	checkedAt := v.CheckedAt
	if checkedAt.IsZero() {
		checkedAt = time.Now().UTC()
	}
	expiresAt := checkedAt.Add(ttl)

	raw := v.RawResponse
	if raw == nil {
		raw = json.RawMessage("null")
	}
	reasonCodes := v.ReasonCodes
	if reasonCodes == nil {
		reasonCodes = []string{}
	}

	row := q.QueryRow(ctx, `
		INSERT INTO screening_results
		  (provider_name, sender_address, risk_score, flagged, reason_codes, raw_response, checked_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id
	`, providerName, address, v.RiskScore, v.Flagged, reasonCodes, []byte(raw), checkedAt, expiresAt)
	if err := row.Scan(&id); err != nil {
		return 0, fmt.Errorf("cache: put: %w", err)
	}
	return id, nil
}

// Invalidate marks every screening_results row for (providerName,
// address) checked at or before this moment as no longer trusted,
// without deleting any of them -- invariant 7's flip side: an operator
// who has reason to distrust a cached result can force a re-check
// without waiting for natural expiry. reason and actor are both
// required -- a silent invalidation with no attribution is not a legal
// call.
//
// The very next Get for this key returns nil immediately after this
// call, even though expires_at on the latest row hasn't naturally
// passed. A subsequent Put naturally has a later checked_at than this
// invalidation, so it is trusted again without needing to "undo"
// anything here.
func Invalidate(ctx context.Context, q Queryer, providerName, address, reason, actor string) error {
	if reason == "" {
		return errors.New("cache: invalidate requires a non-empty reason")
	}
	if actor == "" {
		return errors.New("cache: invalidate requires a non-empty actor")
	}

	_, err := q.Exec(ctx, `
		INSERT INTO screening_result_invalidations (provider_name, sender_address, reason, actor)
		VALUES ($1, $2, $3, $4)
	`, providerName, address, reason, actor)
	if err != nil {
		return fmt.Errorf("cache: invalidate: %w", err)
	}
	slog.Info("cache: invalidated", "provider", providerName, "sender_address", address, "reason", reason, "actor", actor)
	return nil
}

// latestInvalidation returns the most recent invalidated_at for
// (providerName, address), or nil if the key has never been invalidated.
func latestInvalidation(ctx context.Context, q Queryer, providerName, address string) (*time.Time, error) {
	var t time.Time
	err := q.QueryRow(ctx, `
		SELECT invalidated_at
		FROM screening_result_invalidations
		WHERE provider_name = $1 AND sender_address = $2
		ORDER BY invalidated_at DESC
		LIMIT 1
	`, providerName, address).Scan(&t)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cache: checking invalidation history: %w", err)
	}
	return &t, nil
}
