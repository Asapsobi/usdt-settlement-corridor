package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
)

// decodeJSON decodes r's body into v, rejecting unknown fields and
// trailing garbage -- same discipline as C1.8's decodeJSON, minus that
// one's amount-specific type-mismatch mapping: no request body in this
// API ever carries a money amount (GET /orphaned-deposits is the only
// place one appears, and only in a response).
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	err := dec.Decode(v)
	if err == nil {
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
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "field "+typeErr.Field+" has the wrong type"))
		return false
	}

	writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "malformed JSON: "+err.Error()))
	return false
}

// urlParam returns the URL path parameter named key, percent-decoded --
// same reasoning as C1.8's own urlParam: chi matches against the raw,
// still-percent-encoded path, so decoding is this application's job.
func urlParam(w http.ResponseWriter, r *http.Request, key string) (string, bool) {
	decoded, err := url.PathUnescape(chi.URLParam(r, key))
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "malformed "+key+" in URL path"))
		return "", false
	}
	return decoded, true
}
