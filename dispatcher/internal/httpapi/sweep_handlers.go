package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"google.golang.org/protobuf/proto"

	"dispatcher/internal/money"
	"dispatcher/internal/signing"
	"dispatcher/internal/txbuild"
)

// postSweepRequest is admin-panel-build-prompts.md's OC.11: sweep a
// slot's own balance via S1's real signing flow -- destination_address
// and reserve_amount are both required, no hardcoded default, same
// posture every other real-money threshold in this codebase takes (an
// operator must consciously choose how much to leave behind, never a
// silently-assumed number that might strand -- or fail to strand -- the
// slot's own next payout). Reads the slot's LIVE on-chain USDT balance,
// not C1's own ledger-accounting balance for this slot's asset account:
// a sweep exists partly to recover value the ledger doesn't already
// know about, so the ledger's own figure would be the wrong amount to
// sweep.
type postSweepRequest struct {
	DestinationAddress string `json:"destination_address"`
	ReserveAmount      string `json:"reserve_amount"`
}

type sweepResponse struct {
	Status           string `json:"status"` // SIGNED or PENDING
	SigningRequestID int64  `json:"signing_request_id"`
	UnsignedTxHex    string `json:"unsigned_tx_hex,omitempty"` // only on PENDING -- resupply verbatim to /finalize
	TronTxID         string `json:"tron_txid,omitempty"`       // only on SIGNED
	SweptAmount      string `json:"swept_amount"`
}

// postSweepSlot builds a real unsigned TRC20 transfer for slot id's
// entire live balance minus reserve_amount, and requests S1 to sign it
// -- the exact same construction sequence (current block reference ->
// txbuild.BuildTransfer -> txbuild.Digest) and the exact same S1
// request/poll contract (internal/signing.Client) every real payout
// already uses, reused here rather than a second implementation.
// Idempotency-Key is required, same as every other write in this
// system, and is passed straight through to S1's own idempotency guard
// -- a retried call with the same key returns the same signing request,
// never a second real transfer.
func (s *Server) postSweepSlot(w http.ResponseWriter, r *http.Request) {
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if idempotencyKey == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, "invalid_request", "Idempotency-Key header is required"))
		return
	}
	id, ok := urlParamInt(w, r, "id")
	if !ok {
		return
	}
	var req postSweepRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.DestinationAddress == "" || req.ReserveAmount == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, "invalid_request", "destination_address and reserve_amount are both required"))
		return
	}
	reserve, err := money.ParseDecimal(req.ReserveAmount)
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadRequest, "invalid_request", "reserve_amount: "+err.Error()))
		return
	}

	slot, err := s.Slots.Get(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if slot.Status != "ACTIVE" {
		writeAPIError(w, newAPIError(http.StatusConflict, "slot_not_active", fmt.Sprintf("slot %d is %s, not ACTIVE", id, slot.Status)))
		return
	}

	balance, err := liveUSDTBalance(r.Context(), s.TronAPIBaseURL, slot.TronAddress)
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadGateway, "balance_read_failed", "reading live on-chain balance: "+err.Error()))
		return
	}
	amount := balance - reserve
	if amount <= 0 {
		writeAPIError(w, newAPIError(http.StatusBadRequest, "nothing_to_sweep",
			fmt.Sprintf("live balance %s minus reserve %s leaves nothing to sweep", balance.Format(), reserve.Format())))
		return
	}

	unsignedTx, digest, err := s.buildSweepTx(r.Context(), slot.TronAddress, req.DestinationAddress, amount)
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadGateway, "build_failed", "building unsigned transaction: "+err.Error()))
		return
	}

	estimatedUSD := float64(amount) / 1_000_000
	result, err := s.Signing.RequestSignature(r.Context(), id, digest, estimatedUSD, idempotencyKey)
	if err != nil {
		writeErr(w, err)
		return
	}

	resp := sweepResponse{SigningRequestID: result.ID, SweptAmount: amount.Format()}
	if result.Status != signing.StatusSigned {
		resp.Status = string(result.Status)
		resp.UnsignedTxHex = hex.EncodeToString(unsignedTx)
		respondJSON(w, http.StatusAccepted, resp)
		return
	}

	txid, err := broadcastSigned(r.Context(), s.Chain, unsignedTx, result.SignedTx)
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadGateway, "broadcast_failed", err.Error()))
		return
	}
	resp.Status = "SIGNED"
	resp.TronTxID = txid
	respondJSON(w, http.StatusOK, resp)
}

// postSweepFinalize completes a sweep that came back PENDING (over S1's
// auto-sign threshold) once a second approver has approved it via the
// existing S1 approval queue (OC.7) -- polls GetSignature once and, if
// now SIGNED, broadcasts. unsigned_tx_hex must be exactly what the
// original POST .../sweep call returned: dispatchd holds no table of
// in-flight sweeps, so the caller (the console) carries it forward
// rather than this service inventing one for a rare, operator-driven
// action.
func (s *Server) postSweepFinalize(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SigningRequestID int64  `json:"signing_request_id"`
		UnsignedTxHex    string `json:"unsigned_tx_hex"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.SigningRequestID == 0 || req.UnsignedTxHex == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, "invalid_request", "signing_request_id and unsigned_tx_hex are both required"))
		return
	}
	unsignedTx, err := hex.DecodeString(req.UnsignedTxHex)
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadRequest, "invalid_request", "unsigned_tx_hex: "+err.Error()))
		return
	}

	result, err := s.Signing.GetSignature(r.Context(), req.SigningRequestID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if result.Status != signing.StatusSigned {
		respondJSON(w, http.StatusAccepted, sweepResponse{Status: string(result.Status), SigningRequestID: result.ID})
		return
	}

	txid, err := broadcastSigned(r.Context(), s.Chain, unsignedTx, result.SignedTx)
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadGateway, "broadcast_failed", err.Error()))
		return
	}
	respondJSON(w, http.StatusOK, sweepResponse{Status: "SIGNED", SigningRequestID: result.ID, TronTxID: txid})
}

// chainRef and chainBroadcaster are the two GrpcBroadcastClient methods
// this file needs -- named here so buildSweepTx/broadcastSigned don't
// have to import the concrete dispatch package type into their own
// signatures, matching how this file already takes *signing.Client by
// its own concrete type (no interface indirection was needed there
// either; kept consistent).
type chainRef interface {
	CurrentBlockReference(ctx context.Context) (txbuild.BlockReference, error)
}

func (s *Server) buildSweepTx(ctx context.Context, from, to string, amount money.Amount) ([]byte, [32]byte, error) {
	ref, err := chainRef(s.Chain).CurrentBlockReference(ctx)
	if err != nil {
		return nil, [32]byte{}, fmt.Errorf("fetching current block reference: %w", err)
	}
	unsignedTx, err := txbuild.BuildTransfer(from, to, amount, ref)
	if err != nil {
		return nil, [32]byte{}, err
	}
	return unsignedTx, txbuild.Digest(unsignedTx), nil
}

type chainBroadcaster interface {
	Broadcast(ctx context.Context, tx *core.Transaction) (string, error)
}

func broadcastSigned(ctx context.Context, chain chainBroadcaster, unsignedTx []byte, signedTx [65]byte) (string, error) {
	var raw core.TransactionRaw
	if err := proto.Unmarshal(unsignedTx, &raw); err != nil {
		return "", fmt.Errorf("re-parsing unsigned tx: %w", err)
	}
	tx := &core.Transaction{RawData: &raw, Signature: [][]byte{signedTx[:]}}
	return chain.Broadcast(ctx, tx)
}

// liveUSDTBalance reads address's live USDT-TRC20 balance directly off
// TronGrid's own REST account endpoint -- the same read
// dispatcher/cmd/sweep-trc20's own usdtBalance helper makes, duplicated
// here (not imported) because that helper lives in package main of a
// separate command, not an importable package; the request/response
// shape and contract address are identical, verified against the same
// real TronGrid endpoint.
func liveUSDTBalance(ctx context.Context, apiBaseURL, address string) (money.Amount, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(apiBaseURL, "/")+"/v1/accounts/"+address, nil)
	if err != nil {
		return 0, err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	var payload struct {
		Data []struct {
			Trc20 []map[string]string `json:"trc20"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, fmt.Errorf("decoding TronGrid response: %w", err)
	}
	if len(payload.Data) == 0 {
		// A TRON address with no on-chain activity ever (nothing sent to
		// it, not even TRX) has no "account" from the chain's own
		// perspective -- TronGrid reports this as success:true, data:[],
		// not an error. Zero balance, not a read failure: a freshly
		// registered slot that hasn't received its first funding yet is
		// exactly this case, confirmed live against a real such address.
		return 0, nil
	}
	for _, entry := range payload.Data[0].Trc20 {
		if raw, ok := entry[txbuild.USDTContractAddress]; ok {
			units, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parsing USDT balance %q: %w", raw, err)
			}
			return money.Amount(units), nil
		}
	}
	return 0, nil // no USDT entry at all means a zero balance, not an error
}
