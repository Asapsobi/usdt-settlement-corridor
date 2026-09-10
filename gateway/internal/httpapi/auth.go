// C6.1: customer auth and rate limiting. Every route from C6.2 onward
// requires a valid, active customer's API key (this file's own
// authMiddleware); write routes additionally require the customer not
// be suspended (requireActiveCustomer) -- a suspended customer can still
// read status and receive already-scheduled webhooks, per this chunk's
// own acceptance criterion, so suspension is a second, narrower gate on
// top of authentication, not folded into it.
package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"gateway/internal/customers"
	"gateway/internal/ratelimit"
)

type contextKey int

const customerContextKey contextKey = iota

// customerFromContext returns the authenticated customer authMiddleware
// placed on the request context. Panics if called on a request that
// never passed through authMiddleware -- a handler bug, not a runtime
// condition to handle gracefully.
func customerFromContext(ctx context.Context) customers.Customer {
	c, ok := ctx.Value(customerContextKey).(customers.Customer)
	if !ok {
		panic("httpapi: customerFromContext called without authMiddleware having run")
	}
	return c
}

// authMiddleware resolves the bearer token to a customer (401 if
// missing/invalid), applies this customer's own rate limit (429 with
// Retry-After if exceeded -- checked BEFORE any handler work, so an
// over-limit caller never pays for work this service then discards),
// and places the resolved customer.Customer on the request context. The
// raw API key itself is never logged, never included in any error
// message, and never returned by any endpoint after creation -- C6.1's
// own acceptance criterion; grepped for directly in this package's own
// tests, not just eyeballed.
func authMiddleware(store *customers.Store, limiter *ratelimit.Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || token == "" {
				writeAPIError(w, errUnauthorized)
				return
			}
			customer, err := store.GetByAPIKey(r.Context(), token)
			if err != nil {
				writeAPIError(w, errUnauthorized)
				return
			}

			if !limiter.Allow(customer.ID, customer.RateLimitPerMinute) {
				retryAfter := ratelimit.RetryAfter(customer.RateLimitPerMinute)
				w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
				writeAPIError(w, newAPIError(http.StatusTooManyRequests, "rate_limited", "too many requests"))
				return
			}

			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), customerContextKey, customer)))
		})
	}
}

// requireProductionCustomer rejects a sandbox API key on a production
// route with 403 -- C6.7's own invariant 4, enforced at the boundary
// rather than trusted implicitly: a sandbox key must be structurally
// incapable of reaching a real C1/C2 call, not just conventionally
// discouraged from it.
func requireProductionCustomer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if customerFromContext(r.Context()).IsSandbox {
			writeAPIError(w, errProductionOnly)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireSandboxCustomer is requireProductionCustomer's own mirror:
// rejects a production API key on a sandbox route with 403, so a
// sandbox order can never be created (or read back) using production
// credentials either.
func requireSandboxCustomer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !customerFromContext(r.Context()).IsSandbox {
			writeAPIError(w, errSandboxOnly)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireActiveCustomer rejects a suspended customer's write with 403 --
// stacked after authMiddleware on write routes only (C6.2's quote
// issuance, C6.3's order creation); read routes (C6.5's status, webhook
// delivery already in flight) never apply this gate, per C6.1's own
// acceptance criterion.
func requireActiveCustomer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if customerFromContext(r.Context()).Status != customers.StatusActive {
			writeAPIError(w, errSuspended)
			return
		}
		next.ServeHTTP(w, r)
	})
}
