package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// decodeJSON decodes r's body into v, rejecting unknown fields and
// trailing garbage -- same discipline as C1.8's and C2.9's own
// decodeJSON. No request body in this API ever carries a money amount,
// so there's no amount-specific type-mismatch mapping to replicate.
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
// same reasoning as C1.8's and C2.9's own urlParam: chi matches against
// the raw, still-percent-encoded path, so decoding is this
// application's job.
func urlParam(w http.ResponseWriter, r *http.Request, key string) (string, bool) {
	decoded, err := url.PathUnescape(chi.URLParam(r, key))
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "malformed "+key+" in URL path"))
		return "", false
	}
	return decoded, true
}

// urlParamInt64 is urlParam plus base-10 integer parsing, for the
// {id} path parameters every hold/screening-result/rescreen-flag
// endpoint takes.
func urlParamInt64(w http.ResponseWriter, r *http.Request, key string) (int64, bool) {
	raw, ok := urlParam(w, r, key)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "malformed "+key+" in URL path"))
		return 0, false
	}
	return id, true
}
