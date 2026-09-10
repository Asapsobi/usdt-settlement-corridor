package httpapi

import (
	"context"
	"net/http"
)

const idempotencyKeyContextKey contextKey = 1

// requireIdempotencyKey rejects a write request with no Idempotency-Key
// header before any handler runs -- "no header, no write," applied
// uniformly to every customer write route, invariant 2's own "extended
// one hop further out to the actual customer's own retries."
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
