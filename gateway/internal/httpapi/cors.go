package httpapi

import "net/http"

// corsMiddleware lets this API be called directly from a browser --
// same reasoning as every prior component's own corsMiddleware.
// Wildcarding Access-Control-Allow-Origin is safe specifically because
// auth here is a header the caller's own JS sets explicitly
// (Authorization: Bearer ...), never a cookie the browser attaches on
// its own -- there's no ambient credential for a permissive origin to
// leak.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Idempotency-Key")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
