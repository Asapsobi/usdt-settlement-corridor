package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
)

type contextKey int

const (
	actorContextKey contextKey = iota
	idempotencyKeyContextKey
)

// AuthConfig maps bearer tokens to the service identity (actor) that
// token authenticates as. Service-to-service only -- there is no
// customer-facing auth here and no per-customer scoping; C6 owns that.
//
// Every handler reads the caller's identity via actorFromContext, never
// the raw Authorization header -- that indirection is what would let
// mTLS (or anything else that ultimately resolves "which service is
// this") replace bearer tokens without touching a single handler.
type AuthConfig struct {
	Tokens map[string]string // token -> actor
}

// AuthConfigFromEnv reads LEDGER_API_TOKENS, formatted as
// "token1:actor1,token2:actor2". At least one usable pair is required.
func AuthConfigFromEnv() (AuthConfig, error) {
	raw := os.Getenv("LEDGER_API_TOKENS")
	if raw == "" {
		return AuthConfig{}, fmt.Errorf("httpapi: LEDGER_API_TOKENS is not set")
	}
	tokens := make(map[string]string)
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		token, actor, ok := strings.Cut(pair, ":")
		if !ok || token == "" || actor == "" {
			return AuthConfig{}, fmt.Errorf("httpapi: malformed LEDGER_API_TOKENS entry %q, want token:actor", pair)
		}
		tokens[token] = actor
	}
	if len(tokens) == 0 {
		return AuthConfig{}, fmt.Errorf("httpapi: LEDGER_API_TOKENS contained no usable token:actor pairs")
	}
	return AuthConfig{Tokens: tokens}, nil
}

func authMiddleware(cfg AuthConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || token == "" {
				writeAPIError(w, errUnauthorized)
				return
			}
			actor, ok := cfg.Tokens[token]
			if !ok {
				writeAPIError(w, errUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorContextKey, actor)))
		})
	}
}

func actorFromContext(ctx context.Context) string {
	actor, _ := ctx.Value(actorContextKey).(string)
	return actor
}

// requireIdempotencyKey rejects a write request with no Idempotency-Key
// header before any handler runs -- "no header, no write," applied
// uniformly to every write route regardless of whether the underlying
// operation has its own idempotency semantics. POST /entries is the one
// route that actually threads the header value into an
// EntryRequest.IdempotencyKey; the others (orders, transitions,
// snapshots, halt) rely on their own natural keys (external_id's
// uniqueness, etc.) and just need the header present, per the blanket
// rule, without this chunk redesigning those lower layers to have their
// own replay semantics.
func requireIdempotencyKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "Idempotency-Key header is required"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), idempotencyKeyContextKey, key)))
	})
}

func idempotencyKeyFromContext(ctx context.Context) string {
	key, _ := ctx.Value(idempotencyKeyContextKey).(string)
	return key
}
