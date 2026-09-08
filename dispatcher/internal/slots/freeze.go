// Freeze detection: verified against real TRON, not assumed. USDT-TRC20
// (like every other Tether-issued token) implements
// isBlackListed(address) -- confirmed live against the real mainnet
// contract (TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t) via a real, free,
// read-only triggerconstantcontract call: selector e47d6060, a
// constant_result of 32 zero bytes for an address known not to be
// blacklisted. This is the real signal C5.9's own build spec asked to be
// confirmed rather than assumed -- Tether's own freeze mechanism is a
// contract-level flag a constant call can read directly, not a
// distinguishable-error-code or account-flag shape at the RPC layer the
// build spec's own phrasing considered as an alternative.
package slots

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fbsobreira/gotron-sdk/pkg/abi"
	"github.com/fbsobreira/gotron-sdk/pkg/address"

	"dispatcher/internal/txbuild"
)

// FreezeChecker is this package's only path to a real TRON node for
// checking Tether's own blacklist flag -- narrow and consumer-defined,
// one read-only call.
type FreezeChecker interface {
	IsBlackListed(ctx context.Context, tronAddress string) (bool, error)
}

// DetectFreeze reports whether slot's own TRON address has been
// Tether-blacklisted -- a frozen slot, caught here before (or shortly
// after) a broadcast that used it, per C5.9's own "recoverable, not a
// loss" framing when caught pre-settlement.
func DetectFreeze(ctx context.Context, slot Slot, chain FreezeChecker) (bool, error) {
	return chain.IsBlackListed(ctx, slot.TronAddress)
}

const isBlackListedMethod = "isBlackListed(address)"

// HTTPFreezeChecker implements FreezeChecker against a real TRON node's
// triggerconstantcontract HTTP surface -- verified live, the same
// discipline txbuild's own real-node checks used.
type HTTPFreezeChecker struct {
	baseURL string
	http    *http.Client
}

// NewHTTPFreezeChecker returns an HTTPFreezeChecker for baseURL (e.g.
// "https://api.trongrid.io").
func NewHTTPFreezeChecker(baseURL string) *HTTPFreezeChecker {
	return &HTTPFreezeChecker{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

type triggerConstantContractRequest struct {
	OwnerAddress     string `json:"owner_address"`
	ContractAddress  string `json:"contract_address"`
	FunctionSelector string `json:"function_selector"`
	Parameter        string `json:"parameter"`
	Visible          bool   `json:"visible"`
}

type triggerConstantContractResponse struct {
	ConstantResult []string `json:"constant_result"`
	Result         struct {
		Result  bool   `json:"result"`
		Message string `json:"message"`
	} `json:"result"`
}

// IsBlackListed calls isBlackListed(tronAddress) on the real USDT-TRC20
// contract via /wallet/triggerconstantcontract -- a free, read-only
// call, no fee_limit, no chain state change.
func (c *HTTPFreezeChecker) IsBlackListed(ctx context.Context, tronAddress string) (bool, error) {
	addr, err := address.Base58ToAddress(tronAddress)
	if err != nil {
		return false, fmt.Errorf("slots: invalid TRON address %q: %w", tronAddress, err)
	}

	// abi.Pack's own output is selector(4) || padded-address-param(32);
	// triggerconstantcontract's real JSON contract wants them split
	// (function_selector as the method string, parameter as only the
	// argument bytes) -- confirmed against a live call, see this file's
	// own package doc comment.
	data, err := abi.Pack(isBlackListedMethod, []abi.Param{{"address": addr.String()}})
	if err != nil {
		return false, fmt.Errorf("slots: encoding isBlackListed calldata: %w", err)
	}
	if len(data) < 4 {
		return false, fmt.Errorf("slots: internal error: isBlackListed calldata is only %d bytes", len(data))
	}

	body := triggerConstantContractRequest{
		OwnerAddress:     tronAddress,
		ContractAddress:  txbuild.USDTContractAddress,
		FunctionSelector: isBlackListedMethod,
		Parameter:        hex.EncodeToString(data[4:]),
		Visible:          true,
	}
	b, err := json.Marshal(body)
	if err != nil {
		return false, fmt.Errorf("slots: encoding freeze-check request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/wallet/triggerconstantcontract", strings.NewReader(string(b)))
	if err != nil {
		return false, fmt.Errorf("slots: building freeze-check request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("slots: checking freeze status for %s: %w", tronAddress, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("slots: reading freeze-check response for %s: %w", tronAddress, err)
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("slots: TRON node returned %d checking freeze status for %s", resp.StatusCode, tronAddress)
	}

	var out triggerConstantContractResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return false, fmt.Errorf("slots: decoding freeze-check response for %s: %w", tronAddress, err)
	}
	if !out.Result.Result {
		return false, fmt.Errorf("slots: TRON node rejected the freeze-check call for %s: %s", tronAddress, out.Result.Message)
	}
	if len(out.ConstantResult) != 1 {
		return false, fmt.Errorf("slots: unexpected constant_result shape for %s: %v", tronAddress, out.ConstantResult)
	}

	resultBytes, err := hex.DecodeString(out.ConstantResult[0])
	if err != nil {
		return false, fmt.Errorf("slots: decoding constant_result for %s: %w", tronAddress, err)
	}
	for _, b := range resultBytes {
		if b != 0 {
			return true, nil
		}
	}
	return false, nil
}
