package ledgerclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// DispatchFailureReporter reverses a dispatch's conversion entry and
// transitions its order dispatching -> held -- C5's own non-retryable
// failure path (invariant 4). Two implementations, behind one interface
// so swapping between them is a config change, not a code change: see
// TwoCallReporter (what actually runs against C1 today) and
// AtomicReporter (built against the endpoint "Read this third" proposes,
// which does not exist in C1 yet).
type DispatchFailureReporter interface {
	ReportDispatchFailure(ctx context.Context, order Order, conversionEntryKey, reason string) error
}

// TwoCallReporter implements DispatchFailureReporter against C1's real,
// currently-shipped surface -- two separate HTTP calls, not atomic. A
// crash between them leaves the reversal posted (correctly balanced,
// on its own) but the order still showing `dispatching`; the next call
// to ReportDispatchFailure for the same order -- in practice, C5.7's own
// reconciliation job finding it, not a human -- safely completes the
// second call without ever re-posting the reversal, because PostReversal
// itself already treats "already reversed" (C1's 409 response, which
// embeds the existing reversal) as success, not an error to retry past.
type TwoCallReporter struct {
	Client *Client
}

// NewTwoCallReporter wires a TwoCallReporter around an existing Client.
func NewTwoCallReporter(c *Client) *TwoCallReporter {
	return &TwoCallReporter{Client: c}
}

// ReportDispatchFailure resolves the conversion entry's numeric id,
// reverses it (idempotently, per PostReversal's own 409 handling), and
// transitions order to `held` citing that reversal as the transition's
// cause. expected_version conflicts are refetched and retried once, the
// same discipline EnterDispatching and ConfirmFinality already use --
// dispatching -> held is NOT halt-blocked (per C1.5's own transition
// table), so there is no halt-backoff branch here, unlike those two.
func (r *TwoCallReporter) ReportDispatchFailure(ctx context.Context, order Order, conversionEntryKey, reason string) error {
	entry, err := r.Client.GetEntryByIdempotencyKey(ctx, conversionEntryKey)
	if err != nil {
		return fmt.Errorf("ledgerclient: resolving conversion entry %q: %w", conversionEntryKey, err)
	}

	occurredAt := time.Now().UTC()
	reversal, _, err := r.Client.PostReversal(ctx, entry.ID, reason, occurredAt)
	if err != nil {
		return fmt.Errorf("ledgerclient: reversing conversion entry %d: %w", entry.ID, err)
	}

	failKey := fmt.Sprintf("dispatcher:fail:%d", order.ID)
	expectedVersion := order.Version
	versionConflictRetried := false
	for {
		_, err := r.Client.TransitionWithEntryID(ctx, order.ExternalID, "held", expectedVersion, reason, reversal.ID, occurredAt, failKey)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrVersionConflict) && !versionConflictRetried {
			versionConflictRetried = true
			refetched, getErr := r.Client.GetOrder(ctx, order.ExternalID)
			if getErr != nil {
				return fmt.Errorf("ledgerclient: refetching order %s after version conflict: %w", order.ExternalID, getErr)
			}
			expectedVersion = refetched.Version
			continue
		}
		return fmt.Errorf("ledgerclient: transitioning order %s to held: %w", order.ExternalID, err)
	}
}

// AtomicReporter implements DispatchFailureReporter against "Read this
// third"'s proposed POST /v1/orders/{external_id}/dispatch-failure
// endpoint -- one call, reversing the conversion entry and transitioning
// to held atomically, server-side. This endpoint does NOT exist in C1
// today; AtomicReporter exists so the swap to it, once it ships, is a
// config change (which DispatchFailureReporter implementation gets
// wired into cmd/dispatchd) rather than new code, and is tested here only
// against a fake server built to the proposed shape -- never against a
// real C1, which cannot serve it.
type AtomicReporter struct {
	Client *Client
}

// NewAtomicReporter wires an AtomicReporter around an existing Client.
func NewAtomicReporter(c *Client) *AtomicReporter {
	return &AtomicReporter{Client: c}
}

type postDispatchFailureRequest struct {
	ConversionEntryIdempotencyKey string `json:"conversion_entry_idempotency_key"`
	Reason                        string `json:"reason"`
}

func (r *AtomicReporter) ReportDispatchFailure(ctx context.Context, order Order, conversionEntryKey, reason string) error {
	idemKey := fmt.Sprintf("dispatcher:fail:%d", order.ID)
	status, body, err := r.Client.do(ctx, http.MethodPost, "/v1/orders/"+order.ExternalID+"/dispatch-failure", idemKey, postDispatchFailureRequest{
		ConversionEntryIdempotencyKey: conversionEntryKey, Reason: reason,
	})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return classify(decodeAPIError(status, body))
	}
	var resp orderResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("ledgerclient: decoding dispatch-failure response for %s: %w", order.ExternalID, err)
	}
	return nil
}
