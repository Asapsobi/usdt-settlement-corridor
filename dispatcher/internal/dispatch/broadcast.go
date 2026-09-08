package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	"dispatcher/internal/signing"
	"dispatcher/internal/txbuild"
)

// SigningService is the narrow slice of S1's real, shipped signing
// contract this package needs -- signing.Client's own RequestSignature
// method, reused directly (its request/response types included) rather
// than re-declared, so *signing.Client and *signing.FakeSigningService
// both satisfy this interface with no adapter. Request/poll, not a
// single synchronous call: S1's own signing requests can come back
// PENDING, awaiting a human approver, and Broadcast surfaces that as
// ErrSignaturePending rather than blocking -- see Broadcast's own doc
// comment for who polls.
type SigningService interface {
	RequestSignature(ctx context.Context, slotID int, digest [32]byte, estimatedUSD float64, idempotencyKey string) (signing.SigningRequest, error)
}

// BroadcastClient is this package's only path to a real TRON node for
// actually broadcasting a signed transaction -- narrow and
// consumer-defined, matching gotron-sdk's own GrpcClient.Broadcast
// signature shape (a *core.Transaction in, a txid out) rather than
// inventing a byte-blob wrapper around it.
type BroadcastClient interface {
	Broadcast(ctx context.Context, tx *core.Transaction) (txid string, err error)
}

// ErrSignaturePending means S1 has not yet produced a signature for this
// attempt (awaiting approval) -- not a failure, and not something
// Broadcast retries itself in a loop (S1's own approval latency can be
// minutes to hours); the caller is expected to re-invoke Broadcast later,
// which will find the SAME dispatch_attempts row (still BUILT) and
// re-request the SAME idempotency key, replaying into SIGNED the moment
// S1 actually has a signature.
var ErrSignaturePending = fmt.Errorf("dispatch: signature is still pending approval")

// ErrSignatureRejected means an approver rejected this attempt's signing
// request -- terminal for this specific attempt (a new one, with a new
// attempt_number, is a fresh decision, not an automatic retry of a
// rejected one).
var ErrSignatureRejected = fmt.Errorf("dispatch: signature request was rejected")

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Broadcast advances one (orderID, attemptNumber) dispatch attempt from
// however far it has already gotten toward BROADCAST. unsignedTx is
// C5.4's own txbuild.BuildTransfer output for this exact attempt --
// content-addressed by its own SHA256, which is what makes calling
// Broadcast again for the SAME logical transfer (a genuine retry, a
// concurrent racing call, or a resume after a crash) safe:
//
//   - A row not yet recorded: INSERT it (BUILT) first, before calling
//     Sign at all -- the UNIQUE constraint on unsigned_tx_hash is what a
//     concurrent duplicate call collides on, not application-level
//     locking.
//   - A row already BROADCAST, CONFIRMED, or FAILED: returned as-is.
//     Never re-signed, never re-broadcast.
//   - A row already SIGNED (a crash landed between the signature coming
//     back and the broadcast call): the STORED signature (dispatch_attempts
//     .signed_tx, the actual bytes, not just signed_tx_hash) is reused
//     directly -- RequestSignature is never called again for this attempt.
//     This is the literal requirement, not just an idempotent-replay
//     nicety: a second signature over the same payload would be a second
//     valid TRON transaction if the first broadcast actually landed
//     despite this process never observing the response, so this path
//     does not get to lean on S1's own idempotency as a safety net. This
//     is also why signing and broadcasting are two SEPARATE commits
//     (ensureSigned, then ensureBroadcast), not one transaction: a crash
//     between them must leave the SIGNED status durably committed, not
//     rolled back to BUILT.
//   - A row still BUILT: request a signature (idempotent on
//     "dispatcher:sign:<order_id>:<attempt>", invariant 6), store the
//     actual bytes, mark SIGNED -- ensureSigned's own guard
//     ("WHERE status = BUILT") means two concurrent callers can both
//     safely reach this point; only one's UPDATE actually lands.
//
// The broadcast step itself runs under a Postgres row lock (SELECT ...
// FOR UPDATE, held only for that step), which is what makes exactly one
// of two concurrent callers for the same attempt actually reach
// chain.Broadcast: the other blocks on the lock, then observes the
// finished row once unblocked and returns it without touching the chain.
func (d *Dispatcher) Broadcast(ctx context.Context, orderID int64, slotID, attemptNumber int, unsignedTx []byte, estimatedUSD float64, chain BroadcastClient) (BroadcastAttempt, error) {
	unsignedHash := hashHex(unsignedTx)

	attempt, err := d.Attempts.Create(ctx, orderID, slotID, attemptNumber, unsignedHash)
	if err != nil {
		return BroadcastAttempt{}, err
	}
	if attempt.Status.terminal() {
		return attempt, nil
	}

	signed, err := d.ensureSigned(ctx, attempt.ID, orderID, slotID, attemptNumber, unsignedTx, estimatedUSD)
	if err != nil {
		return BroadcastAttempt{}, err
	}
	if signed.Status.terminal() {
		return signed, nil
	}

	return d.ensureBroadcast(ctx, signed.ID, unsignedTx, chain)
}

// ensureSigned brings attemptID to (at least) SIGNED, requesting a
// signature only if it is not already there -- see Broadcast's own doc
// comment for why this is a separate, separately-committed step from
// ensureBroadcast.
func (d *Dispatcher) ensureSigned(ctx context.Context, attemptID, orderID int64, slotID, attemptNumber int, unsignedTx []byte, estimatedUSD float64) (BroadcastAttempt, error) {
	current, err := d.Attempts.Get(ctx, attemptID)
	if err != nil {
		return BroadcastAttempt{}, err
	}
	if current.Status.terminal() || current.Status == BroadcastSigned {
		return current, nil
	}

	digest := txbuild.Digest(unsignedTx)
	signIdemKey := fmt.Sprintf("dispatcher:sign:%d:%d", orderID, attemptNumber)
	sigReq, err := d.Signer.RequestSignature(ctx, slotID, digest, estimatedUSD, signIdemKey)
	if err != nil {
		return BroadcastAttempt{}, fmt.Errorf("dispatch: requesting signature for order %d attempt %d: %w", orderID, attemptNumber, err)
	}
	switch sigReq.Status {
	case signing.StatusPending:
		return BroadcastAttempt{}, ErrSignaturePending
	case signing.StatusRejected:
		// Terminal for this specific attempt -- marked FAILED here,
		// immediately, rather than left for C5.7's reconciliation job to
		// infer later: a rejection is an unambiguous fact the moment S1
		// reports it, not something worth a scan-and-guess pass.
		if _, markErr := d.Attempts.MarkFailed(ctx, attemptID); markErr != nil {
			return BroadcastAttempt{}, fmt.Errorf("dispatch: marking attempt %d FAILED after signature rejection: %w", attemptID, markErr)
		}
		return BroadcastAttempt{}, ErrSignatureRejected
	}

	signatureHex := hex.EncodeToString(sigReq.SignedTx[:])
	return d.Attempts.markSigned(ctx, attemptID, signatureHex, hashHex(sigReq.SignedTx[:]))
}

// ensureBroadcast brings a SIGNED attemptID to BROADCAST, using its own
// stored signature -- never a freshly requested one.
func (d *Dispatcher) ensureBroadcast(ctx context.Context, attemptID int64, unsignedTx []byte, chain BroadcastClient) (BroadcastAttempt, error) {
	var result BroadcastAttempt
	err := d.Attempts.withTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		current, err := d.Attempts.getForUpdate(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		if current.Status.terminal() {
			result = current
			return nil
		}
		if current.Status != BroadcastSigned || current.SignedTx == nil {
			return fmt.Errorf("dispatch: broadcast attempt %d is not SIGNED with a stored signature (status %s)", attemptID, current.Status)
		}

		signature, err := hex.DecodeString(*current.SignedTx)
		if err != nil || len(signature) != 65 {
			return fmt.Errorf("dispatch: stored signature for attempt %d is not 65 bytes of hex: %v", attemptID, err)
		}
		var sigArray [65]byte
		copy(sigArray[:], signature)

		fullTx, err := reconstructTransaction(unsignedTx, sigArray)
		if err != nil {
			return fmt.Errorf("dispatch: reconstructing signed transaction for attempt %d: %w", attemptID, err)
		}
		tronTxID, err := chain.Broadcast(ctx, fullTx)
		if err != nil {
			return fmt.Errorf("dispatch: broadcasting attempt %d: %w", attemptID, err)
		}

		result, err = d.Attempts.markBroadcast(ctx, tx, attemptID, tronTxID, time.Now().UTC())
		return err
	})
	if err != nil {
		return BroadcastAttempt{}, err
	}
	return result, nil
}

// reconstructTransaction rebuilds the full, broadcast-ready
// core.Transaction from unsignedTx (raw_data's own marshaled bytes,
// proto.Unmarshal's exact inverse of what BuildTransfer produced) and
// the 65-byte compact signature S1 returned -- the same signature shape
// TRON's own broadcast surface expects (R || S || recovery byte,
// standard secp256k1 compact form).
func reconstructTransaction(unsignedTx []byte, signature [65]byte) (*core.Transaction, error) {
	raw := &core.TransactionRaw{}
	if err := proto.Unmarshal(unsignedTx, raw); err != nil {
		return nil, fmt.Errorf("unmarshaling raw_data: %w", err)
	}
	return &core.Transaction{
		RawData:   raw,
		Signature: [][]byte{signature[:]},
	}, nil
}
