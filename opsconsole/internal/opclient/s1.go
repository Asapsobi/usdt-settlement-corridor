package opclient

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// S1Client calls one S1 (key management/signing) instance. Reads use the
// console's own shared C5-scope token; approve/reject take a PER-OPERATOR
// approver token instead (see OC.7's own doc comment and this package's
// own Approve/Reject signatures) -- S1 must never see the console's own
// identity on an approval, only the individual human's.
type S1Client struct {
	baseURL string
	c5Token string
	http    *http.Client
}

// NewS1Client returns an S1Client for baseURL, using c5Token for every
// read (RequestSignature-scope routes only -- approve/reject never use
// this token, see Approve/Reject).
func NewS1Client(baseURL, c5Token string) *S1Client {
	return &S1Client{baseURL: strings.TrimRight(baseURL, "/"), c5Token: c5Token, http: &http.Client{Timeout: 5 * time.Second}}
}

// Healthz reports whether S1's own /healthz responds 200.
func (c *S1Client) Healthz(ctx context.Context) error {
	return checkHealthz(ctx, c.http, "s1", c.baseURL)
}

// PendingSigningRequest is S1's own GET /v1/signing-requests?status=pending
// response shape (s1/internal/httpapi/signing_handlers.go's own
// pendingSummaryResponse, added by this same build -- see OC.7).
type PendingSigningRequest struct {
	ID           int64     `json:"id"`
	SlotID       int       `json:"slot_id"`
	EstimatedUSD float64   `json:"estimated_usd"`
	CreatedAt    time.Time `json:"created_at"`
}

// ListPendingApprovals reads S1's pending signing-request queue, using
// the console's own shared C5-scope token -- a read, fine to share (see
// invariant 1's own scope: only approve/reject is not).
func (c *S1Client) ListPendingApprovals(ctx context.Context) ([]PendingSigningRequest, error) {
	var out struct {
		SigningRequests []PendingSigningRequest `json:"signing_requests"`
	}
	err := do(ctx, c.http, "s1", c.c5Token, http.MethodGet, c.baseURL+"/v1/signing-requests?status=pending", nil, &out)
	return out.SigningRequests, err
}

// SigningRequestResult is S1's own signingRequestResponse shape.
type SigningRequestResult struct {
	ID        int64     `json:"id"`
	Status    string    `json:"status"`
	SignedTx  *string   `json:"signed_tx"`
	CreatedAt time.Time `json:"created_at"`
}

// Approve calls S1's own POST /v1/signing-requests/{id}/approve using
// approverToken -- the OPERATOR'S OWN token, entered at login (OC.1),
// never c.c5Token. S1 records the bearer token's own identity as the
// approver; this is invariant 1 made concrete.
func (c *S1Client) Approve(ctx context.Context, id int64, approverToken string) (SigningRequestResult, error) {
	var out SigningRequestResult
	err := do(ctx, c.http, "s1", approverToken, http.MethodPost, c.baseURL+"/v1/signing-requests/"+strconv.FormatInt(id, 10)+"/approve", nil, &out)
	return out, err
}

// Reject calls S1's own POST /v1/signing-requests/{id}/reject using
// approverToken -- same posture as Approve.
func (c *S1Client) Reject(ctx context.Context, id int64, approverToken string) (SigningRequestResult, error) {
	var out SigningRequestResult
	err := do(ctx, c.http, "s1", approverToken, http.MethodPost, c.baseURL+"/v1/signing-requests/"+strconv.FormatInt(id, 10)+"/reject", nil, &out)
	return out, err
}
