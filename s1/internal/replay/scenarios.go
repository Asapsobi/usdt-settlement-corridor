package replay

import (
	"errors"
	"fmt"
	"sync"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"

	"s1/internal/requests"
)

const (
	compactSigRecoveryBase   = 27
	compactSigCompressedFlag = 4
)

// verifySignature independently re-derives the public key from
// (digest, sig) via ecdsa.RecoverCompact and checks it matches
// expectedPubKey -- the replay harness's own proof that a SIGNED
// request's signature is real, not just that the code claims it is.
func verifySignature(sig [65]byte, digest [32]byte, expectedPubKey [33]byte) (bool, error) {
	expected, err := secp256k1.ParsePubKey(expectedPubKey[:])
	if err != nil {
		return false, err
	}
	var s secp256k1.ModNScalar
	if s.SetByteSlice(sig[32:64]) {
		return false, errors.New("signature S overflows the curve order")
	}
	if s.IsOverHalfOrder() {
		return false, errors.New("signature S is not canonical (over half order)")
	}

	compact := make([]byte, 65)
	compact[0] = compactSigRecoveryBase + sig[64] + compactSigCompressedFlag
	copy(compact[1:33], sig[0:32])
	copy(compact[33:65], sig[32:64])

	recovered, _, err := ecdsa.RecoverCompact(compact, digest[:])
	if err != nil {
		return false, err
	}
	return recovered.IsEqual(expected), nil
}

// scenarioCleanUnderThresholdMajority: N requests, all safely under the
// configured threshold, all auto-sign, every signature independently
// verifies against its own slot's public key.
func (h *harness) scenarioCleanUnderThresholdMajority() Result {
	const name = "CleanUnderThresholdMajorityAllAutoSignAndVerify"
	const n = 10

	slotID, err := h.newSlot("clean-majority")
	if err != nil {
		return fail(name, err)
	}
	pubKey, err := h.signer.GetPublicKey(h.ctx, fmt.Sprintf("clean-majority-%d", slotID))
	if err != nil {
		return fail(name, err)
	}
	store := h.newStore(10000)

	for i := 0; i < n; i++ {
		digest := h.nextDigest()
		req, err := store.RequestSignature(h.ctx, slotID, digest, 5000, h.nextIdemKey("clean"))
		if err != nil {
			return fail(name, err)
		}
		if req.Status != requests.StatusSigned {
			return fail(name, fmt.Errorf("request %d: status = %s, want SIGNED", i, req.Status))
		}
		ok, err := verifySignature(req.SignedTx, digest, pubKey)
		if err != nil {
			return fail(name, fmt.Errorf("request %d: verifying signature: %w", i, err))
		}
		if !ok {
			return fail(name, fmt.Errorf("request %d: signature does not verify against the slot's own public key", i))
		}
	}
	return pass(name)
}

// scenarioOverThresholdTwoApproversSigns: a request at or above threshold
// stays PENDING until two distinct real approvals complete it.
func (h *harness) scenarioOverThresholdTwoApproversSigns() Result {
	const name = "OverThresholdTwoDistinctApproversSigns"

	slotID, err := h.newSlot("two-approvers")
	if err != nil {
		return fail(name, err)
	}
	store := h.newStore(10000)
	digest := h.nextDigest()

	req, err := store.RequestSignature(h.ctx, slotID, digest, 50000, h.nextIdemKey("two-app"))
	if err != nil {
		return fail(name, err)
	}
	if req.Status != requests.StatusPending {
		return fail(name, fmt.Errorf("status = %s, want PENDING", req.Status))
	}

	if _, err := store.Approve(h.ctx, req.ID, "alice"); err != nil {
		return fail(name, err)
	}
	final, err := store.Approve(h.ctx, req.ID, "bob")
	if err != nil {
		return fail(name, err)
	}
	if final.Status != requests.StatusSigned {
		return fail(name, fmt.Errorf("status after two distinct approvals = %s, want SIGNED", final.Status))
	}
	return pass(name)
}

// scenarioOverThresholdRejectVetoes: one REJECT resolves a request
// immediately, regardless of any prior APPROVE, and it never signs.
func (h *harness) scenarioOverThresholdRejectVetoes() Result {
	const name = "OverThresholdOneRejectVetoesRegardlessOfPriorApprovals"

	slotID, err := h.newSlot("reject-veto")
	if err != nil {
		return fail(name, err)
	}
	store := h.newStore(10000)
	digest := h.nextDigest()

	req, err := store.RequestSignature(h.ctx, slotID, digest, 50000, h.nextIdemKey("reject"))
	if err != nil {
		return fail(name, err)
	}
	if _, err := store.Approve(h.ctx, req.ID, "alice"); err != nil {
		return fail(name, err)
	}
	final, err := store.Reject(h.ctx, req.ID, "carol")
	if err != nil {
		return fail(name, err)
	}
	if final.Status != requests.StatusRejected {
		return fail(name, fmt.Errorf("status = %s, want REJECTED", final.Status))
	}
	if final.SignedTx != ([65]byte{}) {
		return fail(name, errors.New("a REJECTED request has a non-empty SignedTx"))
	}
	return pass(name)
}

// scenarioConcurrentDuplicateIdempotencyKey: many concurrent callers
// using the SAME idempotency key under threshold must produce exactly
// one signing request and exactly one real KMS Sign call.
func (h *harness) scenarioConcurrentDuplicateIdempotencyKey() Result {
	const name = "ConcurrentDuplicateIdempotencyKeyUnderThresholdSignsExactlyOnce"
	const n = 20

	slotID, err := h.newSlot("concurrent-dup")
	if err != nil {
		return fail(name, err)
	}
	store := h.newStore(10000)
	digest := h.nextDigest()
	idemKey := h.nextIdemKey("concurrent")

	var wg sync.WaitGroup
	ids := make([]int64, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, err := store.RequestSignature(h.ctx, slotID, digest, 5000, idemKey)
			ids[i], errs[i] = req.ID, err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			return fail(name, fmt.Errorf("goroutine %d: %w", i, err))
		}
	}
	first := ids[0]
	for _, id := range ids {
		if id != first {
			return fail(name, fmt.Errorf("concurrent calls produced different request ids: %v", ids))
		}
	}
	if got := h.fake.SignCallCount(fmt.Sprintf("concurrent-dup-%d", slotID)); got != 1 {
		return fail(name, fmt.Errorf("KMS Sign was called %d times across %d concurrent requests, want exactly 1", got, n))
	}
	return pass(name)
}

// scenarioKMSFailureMidApprovalRecoversOnRetry: a KMS failure on the
// approval that would otherwise complete a request leaves both recorded
// approvals intact (invariant: "a failed Sign never spends the state
// that led to it") -- a subsequent successful approval recovers cleanly.
func (h *harness) scenarioKMSFailureMidApprovalRecoversOnRetry() Result {
	const name = "KMSFailureMidApprovalKeepsApprovalsAndRecoversOnRetry"

	slotID, err := h.newSlot("kms-failure-retry")
	if err != nil {
		return fail(name, err)
	}
	kmsKeyID := fmt.Sprintf("kms-failure-retry-%d", slotID)
	store := h.newStore(10000)
	digest := h.nextDigest()

	req, err := store.RequestSignature(h.ctx, slotID, digest, 50000, h.nextIdemKey("kms-fail"))
	if err != nil {
		return fail(name, err)
	}
	if _, err := store.Approve(h.ctx, req.ID, "alice"); err != nil {
		return fail(name, err)
	}

	h.fake.ForceMalformed(kmsKeyID)
	if _, err := store.Approve(h.ctx, req.ID, "bob"); err == nil {
		return fail(name, errors.New("Approve with a forced KMS failure returned no error"))
	}

	stillPending, err := store.GetSignature(h.ctx, req.ID)
	if err != nil {
		return fail(name, err)
	}
	if stillPending.Status != requests.StatusPending {
		return fail(name, fmt.Errorf("status after a failed sign attempt = %s, want still PENDING", stillPending.Status))
	}

	// Clear the forced failure and retry with a third approver -- the
	// two already-recorded approvals (alice, bob) plus this one should
	// not be required to re-approve from scratch; a third distinct
	// approver alone is enough to re-cross the 2-of-N threshold.
	h.fake.ClearForced(kmsKeyID)
	final, err := store.Approve(h.ctx, req.ID, "carol")
	if err != nil {
		return fail(name, err)
	}
	if final.Status != requests.StatusSigned {
		return fail(name, fmt.Errorf("status after retry = %s, want SIGNED", final.Status))
	}
	return pass(name)
}
