package txbuild

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/fbsobreira/gotron-sdk/pkg/abi"
	"github.com/fbsobreira/gotron-sdk/pkg/address"
	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"dispatcher/internal/money"
)

// MaxMultisendRecipients bounds how many recipients BuildMultisend will
// ever put in one transaction. NOT independently verified against a real
// contract or a real TRON energy/size limit -- there is no real
// multisend contract yet (see this file's own package-level doc note);
// this reuses the number ledger/internal/journal's own maxLines comment
// already settled on for the same reason ("the largest MVP batch (Sweep
// tier) is on the order of 50 recipients"), so this project has exactly
// one assumed Sweep batch ceiling, not two independently-guessed ones.
// Whoever designs and deploys the real contract must re-derive this from
// its own actual gas/energy behavior, not assume this number survives
// unchanged.
const MaxMultisendRecipients = 50

// multisendMethod is the proposed multisend contract's own transfer
// entry point -- modeled directly on disperse.app's own real, widely-used,
// audited Disperse.sol contract (the standard shape for this exact
// problem on EVM-family chains), NOT invented from nothing: disperseToken
// pulls `sum(amounts)` of token from the caller via transferFrom, then
// loops transfer() to each recipient. That transferFrom step means the
// slot address must have approved this contract for at least the
// batch's own total beforehand -- a one-time (or periodically renewed)
// provisioning step, analogous to EnterDispatching's own EnsureAccount
// call, that this package does not build (out of scope for a
// transaction-construction package, and moot until a real contract
// address exists to approve).
const multisendMethod = "disperseToken(address,address[],uint256[])"

var (
	// ErrTooManyRecipients means len(recipients) exceeds
	// MaxMultisendRecipients -- rejected before any construction, never
	// silently truncated.
	ErrTooManyRecipients = errors.New("txbuild: recipient count exceeds the multisend contract's own per-transaction limit")

	// ErrNoRecipients means recipients was empty -- never a valid batch.
	ErrNoRecipients = errors.New("txbuild: at least one recipient is required")
)

// Recipient is one leg of a Sweep-tier batch: one order's own payout
// address and amount.
type Recipient struct {
	Address string
	Amount  money.Amount
}

// BuildMultisend constructs an unsigned call to multisendContractAddress's
// own disperseToken(USDTContractAddress, recipients, amounts) -- C5.8's
// own batched payout, one transaction covering every recipient in the
// batch. multisendContractAddress is NOT a package constant the way
// USDTContractAddress is: the contract this calls does not exist on any
// real network today (decision 1's "no smart contracts" versus decision
// 6's batching-margin claim -- see this project's own C5.8 design
// discussion), so the address is necessarily a caller-supplied,
// provisional value, not something this package could hardcode even if
// it wanted to.
//
// Deterministic and offline, exactly like BuildTransfer, for the same
// reasons (see BuildTransfer's own doc comment) -- and, like
// BuildTransfer, UNVERIFIED against a live triggersmartcontract call:
// there is no real contract to verify the calldata shape against yet.
// The ABI encoding itself (array-of-address, array-of-uint256) goes
// through the same gotron-sdk abi.Pack machinery BuildTransfer's own
// single-address encoding already does, which IS independently proven
// correct for a single address; the array case is the same library's own
// code path, not a separately hand-rolled one.
func BuildMultisend(slotAddress, multisendContractAddress string, recipients []Recipient, ref BlockReference) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, ErrNoRecipients
	}
	if len(recipients) > MaxMultisendRecipients {
		return nil, fmt.Errorf("%w: got %d, max %d", ErrTooManyRecipients, len(recipients), MaxMultisendRecipients)
	}

	owner, err := address.Base58ToAddress(slotAddress)
	if err != nil {
		return nil, fmt.Errorf("%w: slotAddress %q: %v", ErrInvalidAddress, slotAddress, err)
	}
	multisendContract, err := address.Base58ToAddress(multisendContractAddress)
	if err != nil {
		return nil, fmt.Errorf("%w: multisendContractAddress %q: %v", ErrInvalidAddress, multisendContractAddress, err)
	}

	recipientAddrs := make([]interface{}, len(recipients))
	amounts := make([]interface{}, len(recipients))
	for i, r := range recipients {
		if _, err := address.Base58ToAddress(r.Address); err != nil {
			return nil, fmt.Errorf("%w: recipient %d address %q: %v", ErrInvalidAddress, i, r.Address, err)
		}
		if r.Amount <= 0 {
			return nil, fmt.Errorf("%w: recipient %d: got %s", ErrNonPositiveAmount, i, r.Amount.Format())
		}
		recipientAddrs[i] = r.Address
		// The ABI-encoded amount is the raw integer minor-units value
		// (matching buildCalldata's own scalar uint256 case), never
		// money.Amount.Format()'s decimal-point string -- an ABI
		// integer parameter has no decimal point.
		amounts[i] = strconv.FormatInt(int64(r.Amount), 10)
	}

	data, err := abi.Pack(multisendMethod, []abi.Param{
		{"address": USDTContractAddress},
		{"address[]": recipientAddrs},
		{"uint256[]": amounts},
	})
	if err != nil {
		return nil, fmt.Errorf("txbuild: encoding disperseToken calldata: %w", err)
	}

	trigger := &core.TriggerSmartContract{
		OwnerAddress:    owner.Bytes(),
		ContractAddress: multisendContract.Bytes(),
		Data:            data,
	}
	parameter, err := anypb.New(trigger)
	if err != nil {
		return nil, fmt.Errorf("txbuild: wrapping TriggerSmartContract: %w", err)
	}

	raw := &core.TransactionRaw{
		RefBlockBytes: []byte{byte(ref.BlockNumber >> 8), byte(ref.BlockNumber)},
		RefBlockHash:  ref.BlockHash[8:16],
		Expiration:    ref.Expiration.UnixMilli(),
		Contract: []*core.Transaction_Contract{
			{Type: core.Transaction_Contract_TriggerSmartContract, Parameter: parameter},
		},
		Timestamp: ref.Timestamp.UnixMilli(),
		FeeLimit:  defaultFeeLimit,
	}

	out, err := proto.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("txbuild: marshaling raw_data: %w", err)
	}
	return out, nil
}
