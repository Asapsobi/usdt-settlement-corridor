// OC.19: gateway's own admin surface. Every prior route in this
// package authenticates a CUSTOMER's own sk_live_/sk_test_ key
// (authMiddleware in auth.go) -- that is structurally the wrong
// direction of trust for an operator route (a customer key must never
// be able to list every OTHER customer's orders or rotate anyone's
// key), so admin routes get their own, completely separate
// service-token auth, mirroring the Bearer token1:actor1,token2:actor2
// convention every sibling service in this corridor already uses
// (ledger/internal/httpapi/auth.go's own AuthConfig).
package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// AdminAuthConfig holds the service tokens allowed to call /v1/admin/*.
type AdminAuthConfig struct {
	Tokens map[string]string // token -> actor
}

// AdminAuthConfigFromEnv reads GATEWAY_ADMIN_TOKENS, formatted as
// "token1:actor1,token2:actor2" -- same shape as every sibling
// service's own *_API_TOKENS. At least one usable pair is required.
func AdminAuthConfigFromEnv() (AdminAuthConfig, error) {
	raw := os.Getenv("GATEWAY_ADMIN_TOKENS")
	if raw == "" {
		return AdminAuthConfig{}, fmt.Errorf("httpapi: GATEWAY_ADMIN_TOKENS is not set")
	}
	tokens := make(map[string]string)
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		token, actor, ok := strings.Cut(pair, ":")
		if !ok || token == "" || actor == "" {
			return AdminAuthConfig{}, fmt.Errorf("httpapi: malformed GATEWAY_ADMIN_TOKENS entry %q, want token:actor", pair)
		}
		tokens[token] = actor
	}
	if len(tokens) == 0 {
		return AdminAuthConfig{}, fmt.Errorf("httpapi: GATEWAY_ADMIN_TOKENS contained no usable token:actor pairs")
	}
	return AdminAuthConfig{Tokens: tokens}, nil
}

type adminActorContextKey struct{}

func adminActorFromContext(ctx context.Context) string {
	actor, _ := ctx.Value(adminActorContextKey{}).(string)
	return actor
}

// requireAdminAuth resolves the bearer token against cfg.Tokens (401 if
// missing/unknown) and places the resolved actor name on the request
// context. Deliberately never touches customers.Store -- a customer's
// own API key, however production/live it is, is not a valid admin
// token and never will be.
func requireAdminAuth(cfg AdminAuthConfig) func(http.Handler) http.Handler {
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
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), adminActorContextKey{}, actor)))
		})
	}
}
