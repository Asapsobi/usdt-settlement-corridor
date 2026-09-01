package httpapi

import "net/http"

// corsMiddleware lets this API be called directly from a browser -- the
// self-contained HTML testing console this ships with, or any other
// browser-based tool an operator points at a local ledgerd. It changes
// nothing about who's allowed to write: the bearer token remains the only
// thing that authorizes a request, and CORS only controls whether a
// browser is permitted to read a cross-origin response. Wildcarding
// Access-Control-Allow-Origin is safe specifically because auth here is
// a header the caller's own JS sets explicitly (Authorization: Bearer
// ...), never a cookie the browser attaches on its own -- there's no
// ambient credential for a permissive origin to leak.
//
// Registered globally, before any route group, so an OPTIONS preflight
// to any path -- including ones chi has no explicit OPTIONS handler
// for -- gets a clean response instead of a 405 from chi's own routing.
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
