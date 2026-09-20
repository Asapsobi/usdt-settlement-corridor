package httpapi

import (
	"net/http"
	"regexp"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"depositwatcher/internal/addresses"
	"depositwatcher/internal/candidates"
	"depositwatcher/internal/chain"
)

var txHashPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`)

type postConfirmDepositRequest struct {
	TxHash string `json:"tx_hash"`
}

// postConfirmDeposit is POST /v1/addresses/{order_id}/confirm-deposit
// (admin-panel-build-prompts.md's OC.12) -- an operator-supplied "look
// at this specific transaction" trigger, not a manual override that
// skips verification. Locates the block via a primary-provider-only
// read (chain.Pool.TransactionBlockNumber, advisory -- it only says
// WHERE to look), then re-fetches that single block's Transfer logs via
// LogsAt, which still enforces the real 2-provider agreement bar
// (invariant 5) exactly as the background scanner's own range-scan
// does. The matching log is fed into candidates.ProcessLog -- the
// SAME classification function ScanRange itself calls, not a second,
// parallel "manually credited" path -- so this order still respects
// finality/confirmation rules downstream (deposit.detected fires, but
// crediting only happens once finality.Tracker's own polling loop
// promotes it, same as any other deposit) and is naturally idempotent
// (ProcessLog -> Tracker.OnLogObserved dedupes by tx_hash:log_index).
//
// A transaction whose Transfer log pays some OTHER address is rejected
// here before ever reaching ProcessLog -- not because ProcessLog would
// mishandle it (it would just resolve to whichever order that other
// address belongs to, or ignore it as chain noise), but because an
// operator confirming order_id's own deposit typing in the wrong
// tx_hash deserves a clear "that's not this order's address" error,
// not a silent no-op against a different order entirely.
func (s *Server) postConfirmDeposit(w http.ResponseWriter, r *http.Request) {
	orderID, ok := orderIDParam(w, r)
	if !ok {
		return
	}
	var req postConfirmDepositRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.TxHash == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "tx_hash is required"))
		return
	}
	if !txHashPattern.MatchString(req.TxHash) {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "tx_hash must be a 0x-prefixed 32-byte hex transaction hash"))
		return
	}
	if s.ChainPool == nil || s.Tracker == nil || s.Ledger == nil {
		writeAPIError(w, errSystemComponentNotReady)
		return
	}

	wa, err := addresses.GetByOrderID(r.Context(), s.Pool, orderID)
	if err != nil {
		writeErr(w, err)
		return
	}

	txHash := common.HexToHash(req.TxHash)
	blockNumber, err := s.ChainPool.TransactionBlockNumber(r.Context(), txHash)
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadGateway, "tx_not_found", "locating transaction on-chain: "+err.Error()))
		return
	}

	logs, err := s.ChainPool.LogsAt(r.Context(), blockNumber, blockNumber, s.ContractAddress, [][]common.Hash{{s.TransferTopic}})
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadGateway, "upstream_error", "fetching Transfer logs: "+err.Error()))
		return
	}

	var matched *types.Log
	for i := range logs {
		if logs[i].TxHash == txHash {
			matched = &logs[i]
			break
		}
	}
	if matched == nil {
		writeAPIError(w, newAPIError(http.StatusNotFound, "log_not_found",
			"no USDT_BEP20 Transfer log for this tx_hash was found in its own block"))
		return
	}

	_, to, _, err := chain.ParseTransferLog(*matched)
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusUnprocessableEntity, "not_a_transfer", err.Error()))
		return
	}
	if to != common.HexToAddress(string(wa.Address)) {
		writeAPIError(w, newAPIError(http.StatusBadRequest, "wrong_address",
			"this transaction pays "+to.Hex()+", not order "+wa.ExternalID+"'s own deposit address "+string(wa.Address)))
		return
	}

	cfg := candidates.Config{ContractAddress: s.ContractAddress, TransferTopic: s.TransferTopic, DustFloor: s.DustFloor}
	if err := candidates.ProcessLog(r.Context(), s.ChainPool, s.Pool, s.Ledger, s.Tracker, cfg, *matched, nil); err != nil {
		writeAPIError(w, newAPIError(http.StatusInternalServerError, "processing_failed", err.Error()))
		return
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"order_id": orderID, "tx_hash": req.TxHash, "block_number": blockNumber, "accepted": true,
	})
}
