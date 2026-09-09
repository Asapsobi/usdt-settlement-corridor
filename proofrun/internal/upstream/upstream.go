// Package upstream is this proof run's only path to C1, C2, and C5 --
// three small, narrow HTTP clients, one per real component this driver
// calls. Deliberately not full-featured clients (each real component
// already has its own, e.g. dispatcher/internal/ledgerclient): this is
// scaffolding for docs/03-build/mvp-proof-run-plan.md's own proof run,
// not a component to harden, so only the handful of calls the driver
// actually makes are implemented.
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// apiErrorEnvelope mirrors the one error shape every component in this
// project returns (ledger/internal/httpapi/errors.go and its siblings).
type apiErrorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// APIError is a structured error response from an upstream component.
type APIError struct {
	Component string
	Status    int
	Code      string
	Message   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("upstream: %s returned %d %s: %s", e.Component, e.Status, e.Code, e.Message)
}

func decodeAPIError(component string, status int, body []byte) *APIError {
	var envelope apiErrorEnvelope
	_ = json.Unmarshal(body, &envelope)
	return &APIError{Component: component, Status: status, Code: envelope.Error.Code, Message: envelope.Error.Message}
}

// client is the shared do() every one of this package's three clients
// embeds -- same shape as every other component's own internal/*client.
type client struct {
	component string
	baseURL   string
	token     string
	http      *http.Client
}

func newClient(component, baseURL, token string) client {
	return client{
		component: component,
		baseURL:   strings.TrimRight(baseURL, "/"),
		token:     token,
		http:      &http.Client{Timeout: 15 * time.Second},
	}
}

func (c client) do(ctx context.Context, method, path, idempotencyKey string, body any) (status int, respBody []byte, err error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("upstream: encoding request body for %s: %w", c.component, err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("upstream: building request for %s: %w", c.component, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("upstream: %s %s %s: %w", c.component, method, path, err)
	}
	defer resp.Body.Close()
	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("upstream: reading %s response for %s %s: %w", c.component, method, path, err)
	}
	return resp.StatusCode, respBody, nil
}
