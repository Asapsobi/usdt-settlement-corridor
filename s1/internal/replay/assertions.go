package replay

import (
	"context"
	"fmt"
)

// finalAssertions checks properties that must hold across EVERYTHING
// this run did, not any one scenario in isolation -- mirroring every
// prior component's own "final assertions" against real database state.
func (h *harness) finalAssertions(ctx context.Context) ([]Result, error) {
	var out []Result
	out = append(out, h.assertEverySignedRequestHasAnAuditRow(ctx))
	out = append(out, h.assertOverThresholdAuditRowsNameAtLeastTwoRealApprovers(ctx))
	out = append(out, h.assertNoOversizedDigestOrSignatureColumns(ctx))
	return out, nil
}

// assertEverySignedRequestHasAnAuditRow: invariant 4 ("every KMS Sign
// call is logged") made independently checkable -- a SIGNED request with
// no matching signing_audit_log row would mean a signature exists that
// nothing ever recorded, exactly the gap invariant 4 exists to close.
func (h *harness) assertEverySignedRequestHasAnAuditRow(ctx context.Context) Result {
	const name = "FinalAssertion_EverySignedRequestHasAnAuditRow"
	var missing int
	err := h.pool.QueryRow(ctx, `
		SELECT count(*) FROM signing_requests sr
		WHERE sr.status = 'SIGNED'
		  AND NOT EXISTS (SELECT 1 FROM signing_audit_log al WHERE al.signing_request_id = sr.id)
	`).Scan(&missing)
	if err != nil {
		return fail(name, err)
	}
	if missing > 0 {
		return fail(name, fmt.Errorf("%d SIGNED request(s) have no matching signing_audit_log row", missing))
	}
	return pass(name)
}

// assertOverThresholdAuditRowsNameAtLeastTwoRealApprovers: an audit row
// that carries approvers at all (the over-threshold path) must name at
// least 2 -- invariant 3's own wording is "never signs on fewer than 2
// distinct APPROVE decisions," not "exactly 2": a request whose first
// sign attempt failed after 2 approvals and later signed on a retry
// triggered by a THIRD distinct approver (see
// scenarioKMSFailureMidApprovalRecoversOnRetry) legitimately accumulates
// more than 2 real votes by the time it signs, and the audit log
// correctly names all of them, not an artificially truncated 2. Every
// name recorded must still correspond to a real, distinct APPROVE
// decision actually in signing_approvals for that same request.
func (h *harness) assertOverThresholdAuditRowsNameAtLeastTwoRealApprovers(ctx context.Context) Result {
	const name = "FinalAssertion_OverThresholdAuditRowsNameAtLeastTwoRealApprovers"

	rows, err := h.pool.Query(ctx, `
		SELECT signing_request_id, approvers FROM signing_audit_log WHERE approvers IS NOT NULL
	`)
	if err != nil {
		return fail(name, err)
	}
	defer rows.Close()

	for rows.Next() {
		var requestID int64
		var approvers []string
		if err := rows.Scan(&requestID, &approvers); err != nil {
			return fail(name, err)
		}
		if len(approvers) < 2 {
			return fail(name, fmt.Errorf("request %d: audit row names %d approver(s), want at least 2", requestID, len(approvers)))
		}

		var realCount int
		if err := h.pool.QueryRow(ctx, `
			SELECT count(*) FROM signing_approvals
			WHERE signing_request_id = $1 AND decision = 'APPROVE' AND approver = ANY($2)
		`, requestID, approvers).Scan(&realCount); err != nil {
			return fail(name, err)
		}
		if realCount != len(approvers) {
			return fail(name, fmt.Errorf("request %d: audit row names %d approvers %v, but only %d have a real, matching, distinct APPROVE decision", requestID, len(approvers), approvers, realCount))
		}
	}
	if err := rows.Err(); err != nil {
		return fail(name, err)
	}
	return pass(name)
}

// assertNoOversizedDigestOrSignatureColumns independently re-verifies,
// via a fresh SELECT (never trusting that the write-time CHECK
// constraints alone are sufficient proof), that every digest this run
// stored is exactly 32 bytes and every non-null signed_tx is exactly 65
// bytes -- the closest structural stand-in this schema offers for "never
// anything key- or seed-shaped slipped in": nothing in internal/requests
// or internal/httpapi ever receives key material from internal/kmssign
// in the first place (kmssign.Wrapper's own public surface returns only
// digests, public keys, and signatures -- see that package's own
// dependency_test.go for the static half of this guarantee), so this
// assertion's job is proving the columns that DO exist hold exactly the
// shapes the application contract promises, not a free-text scan for
// suspicious bytes.
func (h *harness) assertNoOversizedDigestOrSignatureColumns(ctx context.Context) Result {
	const name = "FinalAssertion_DigestAndSignatureColumnsAreExactlyTheContractedWidth"

	var badDigests, badSignatures int
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*) FROM signing_requests WHERE octet_length(digest) <> 32
	`).Scan(&badDigests); err != nil {
		return fail(name, err)
	}
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*) FROM signing_requests WHERE signed_tx IS NOT NULL AND octet_length(signed_tx) <> 65
	`).Scan(&badSignatures); err != nil {
		return fail(name, err)
	}
	if badDigests > 0 || badSignatures > 0 {
		return fail(name, fmt.Errorf("%d row(s) with a wrong-sized digest, %d with a wrong-sized signed_tx", badDigests, badSignatures))
	}
	return pass(name)
}
