package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
)

// decodeJSON decodes r's body into v, rejecting unknown fields and, most
// importantly, rejecting a JSON number anywhere a Go string field expects
// one. That rejection needs no special-case code here: every amount field
// in every request DTO in this package is declared as a plain Go string
// (parsed into money.Amount afterward via money.ParseDecimal), and
// encoding/json already refuses to unmarshal a JSON number into a string
// field on its own. What this function adds is turning that raw
// *json.UnmarshalTypeError into the stable error code a caller can act
// on: invalid_amount when the offending field looks like an amount,
// invalid_request otherwise. This is not pedantry -- per the build spec,
// silently accepting a JSON number here is the most likely way this
// system loses money, since JSON numbers are floats in most client
// languages.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	err := dec.Decode(v)
	if err == nil {
		// Reject trailing garbage after the JSON value.
		if dec.More() {
			writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "request body must contain exactly one JSON object"))
			return false
		}
		return true
	}

	if err == io.EOF {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "request body must not be empty"))
		return false
	}

	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		if looksLikeAmountField(typeErr.Field) {
			writeAPIError(w, errInvalidAmount)
			return false
		}
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "field "+typeErr.Field+" has the wrong type"))
		return false
	}

	writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "malformed JSON: "+err.Error()))
	return false
}

// urlParam returns the URL path parameter named key, percent-decoded.
// chi's router matches -- and populates URLParam from -- the request's
// raw, still-percent-encoded path whenever the request line contains any
// escaping at all (see routeHTTP in go-chi/chi's mux.go: it prefers
// r.URL.RawPath over the already-decoded r.URL.Path precisely because
// decoding at the routing layer would be unsafe in general -- a %2F could
// collapse into a literal path separator and change which route or
// wildcard boundary matches). Decoding is therefore this application's
// job, once, right where a path parameter is read. Skipping it means a
// well-behaved client that percent-encodes a reserved character in a
// path segment -- a colon in an account code, say, which url.PathEscape
// and JavaScript's encodeURIComponent both do by default -- gets a 404
// for an account that plainly exists, for a request that never reaches
// this handler with the value the client actually sent.
func urlParam(w http.ResponseWriter, r *http.Request, key string) (string, bool) {
	decoded, err := url.PathUnescape(chi.URLParam(r, key))
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "malformed "+key+" in URL path"))
		return "", false
	}
	return decoded, true
}

// looksLikeAmountField reports whether a JSON field path (e.g.
// "lines.0.amount", "amount_in") is plausibly an amount, so a type
// mismatch there maps to invalid_amount instead of the generic
// invalid_request.
func looksLikeAmountField(field string) bool {
	f := strings.ToLower(field)
	return strings.Contains(f, "amount") || strings.HasSuffix(f, "_units")
}
