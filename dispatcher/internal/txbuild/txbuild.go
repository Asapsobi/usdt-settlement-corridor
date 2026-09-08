// Package txbuild constructs unsigned TRC20 transfer transactions. Pure
// construction only: no signing (that's internal/signing, the only
// package with a path to a real key, once one exists), no broadcast, and
// -- deliberately -- no network call of any kind, not even a read-only
// one. That last constraint is the reason BuildTransfer's real signature
// is wider than c5-payout-dispatcher-build-prompts.md's own C5.4 sketch
// (`BuildTransfer(slotAddress, recipientAddress string, amount
// money.Amount)`): a TRON transaction's raw_data needs a recent block
// reference (ref_block_bytes/ref_block_hash) and an expiration, and there
// is no way to obtain either without asking a node -- which this package
// is not allowed to do. BlockReference carries what a caller (who DOES
// get to make that one read-only call, elsewhere) already resolved.
//
// Verified against real TRON, not assumed: every byte-layout claim below
// was checked against a live triggersmartcontract call and a real,
// historical TRC20 transfer fetched from TronGrid's own API (not just
// documentation) -- see the doc comments on buildCalldata and Digest for
// what was specifically confirmed and how.
package txbuild

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/fbsobreira/gotron-sdk/pkg/abi"
	"github.com/fbsobreira/gotron-sdk/pkg/address"
	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"dispatcher/internal/money"
)

// USDTContractAddress is the one real, permanent USDT-TRC20 contract this
// entire corridor exists to pay out from -- a fixed constant, not
// per-order configuration (this system settles exactly one TRC20 asset).
// Verified live: TronGrid's own transaction history for this address
// shows real, current USDT transfer activity.
const USDTContractAddress = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"

// transferMethod is USDT's (and every standard TRC20's) transfer method
// signature. abi.Signature("transfer(address,uint256)") produces
// selector a9059cbb -- confirmed against a live triggersmartcontract
// response for this exact contract.
const transferMethod = "transfer(address,uint256)"

// defaultFeeLimit bounds the energy/bandwidth this transaction may burn
// if the recipient (or, for USDT, Tether's own blacklist check inside
// the contract) makes the call more expensive than the common case.
// 100 TRX in sun, matching the fee_limit this package's own live
// verification calls used.
const defaultFeeLimit = 100_000_000

var (
	// ErrInvalidAddress means slotAddress or recipientAddress failed
	// TRON's own base58check decoding -- wrong length, wrong version
	// byte, or a bad checksum. Returned before any transaction bytes are
	// built, per this chunk's own acceptance criterion.
	ErrInvalidAddress = errors.New("txbuild: invalid TRON address")

	// ErrNonPositiveAmount means amount was zero or negative -- never a
	// valid payout.
	ErrNonPositiveAmount = errors.New("txbuild: amount must be positive")
)

// BlockReference is the recent chain state a caller resolved elsewhere
// (a single read-only node call, outside this package) and supplies here
// so BuildTransfer never has to make one itself. BlockNumber and
// BlockHash come from the same block -- e.g. a real node's own
// getnowblock response, or any block within TRON's ~2-minute expiration
// window.
type BlockReference struct {
	// BlockNumber is the referenced block's height. Only its low 16 bits
	// are ever used (ref_block_bytes), matching TRON's own tolerance for
	// referencing a nearby, not exact, recent block.
	BlockNumber int64

	// BlockHash is that block's own 32-byte blockID. TRON's own blockID
	// format embeds the block number in its first 8 bytes and the actual
	// hash in the remaining 24; ref_block_hash is bytes [8:16] of this
	// value -- the first 8 bytes of the hash portion, not BlockNumber
	// re-encoded, confirmed against a live node response.
	BlockHash [32]byte

	// Timestamp becomes raw_data.timestamp (Unix milliseconds). Expiration
	// becomes raw_data.expiration. Both are part of what gets hashed and
	// signed, so -- like BlockNumber/BlockHash -- a caller retrying the
	// SAME logical dispatch attempt after a crash must supply the SAME
	// values it used the first time, not freshly computed ones, or the
	// rebuilt transaction will not match the one already recorded (see
	// dispatch_attempts.unsigned_tx_hash's own uniqueness contract,
	// C5.5). BuildTransfer itself never reads the clock.
	Timestamp  time.Time
	Expiration time.Time
}

// BuildTransfer constructs an unsigned TRC20 transfer of amount USDT from
// slotAddress to recipientAddress, calling USDTContractAddress. The
// returned bytes are raw_data's own protobuf encoding -- not a full
// Transaction, and not yet a digest: SHA256 of this exact byte slice is
// both this transaction's own txID (TRON's own convention -- confirmed
// live) and the digest internal/signing's SigningService is asked to
// sign (see Digest).
//
// Deterministic: identical arguments produce byte-identical output,
// every time -- protobuf's own field-order-by-number encoding for a
// message with no maps and no repeated field whose order this package
// itself doesn't fix, plus never reading the clock or any other
// process-local state, is what makes that true, not an accident.
//
// slotAddress and recipientAddress are validated (TRON base58check:
// length, version byte, checksum) before anything else runs.
func BuildTransfer(slotAddress, recipientAddress string, amount money.Amount, ref BlockReference) ([]byte, error) {
	owner, err := address.Base58ToAddress(slotAddress)
	if err != nil {
		return nil, fmt.Errorf("%w: slotAddress %q: %v", ErrInvalidAddress, slotAddress, err)
	}
	recipient, err := address.Base58ToAddress(recipientAddress)
	if err != nil {
		return nil, fmt.Errorf("%w: recipientAddress %q: %v", ErrInvalidAddress, recipientAddress, err)
	}
	contract, err := address.Base58ToAddress(USDTContractAddress)
	if err != nil {
		// Unreachable for the real constant; guarded because
		// address.Base58ToAddress always returns an error value, and a
		// silently-invalid contract constant is not a case to ignore.
		return nil, fmt.Errorf("txbuild: internal error: USDTContractAddress is invalid: %w", err)
	}
	if amount <= 0 {
		return nil, fmt.Errorf("%w: got %s", ErrNonPositiveAmount, amount.Format())
	}

	data, err := buildCalldata(recipient.String(), amount)
	if err != nil {
		return nil, fmt.Errorf("txbuild: encoding transfer calldata: %w", err)
	}

	trigger := &core.TriggerSmartContract{
		OwnerAddress:    owner.Bytes(),
		ContractAddress: contract.Bytes(),
		Data:            data,
	}
	parameter, err := anypb.New(trigger)
	if err != nil {
		return nil, fmt.Errorf("txbuild: wrapping TriggerSmartContract: %w", err)
	}

	refBlockBytes := []byte{byte(ref.BlockNumber >> 8), byte(ref.BlockNumber)}
	refBlockHash := ref.BlockHash[8:16]

	raw := &core.TransactionRaw{
		RefBlockBytes: refBlockBytes,
		RefBlockHash:  refBlockHash,
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

// buildCalldata ABI-encodes transfer(address,uint256) via gotron-sdk's
// own abi.Pack -- not a hand-rolled encoder -- so the address parameter's
// encoding rule doesn't have to be re-derived by hand: abi.Pack resolves
// a TRON address (recipient, here) to its base58check-validated bytes
// and, for the "address" ABI type specifically, uses only the LAST 20 of
// those 21 bytes (TRON's own 0x41 network-version byte is stripped,
// standard EVM/Ethereum address width, zero-padded on the left to a
// 32-byte slot) -- confirmed against a real, historical USDT-TRC20
// transfer fetched from TronGrid: its calldata's recipient parameter is
// exactly 12 zero bytes followed by a 20-byte value, never 21.
//
// amount is USDT's own smallest unit (10^-6 USDT), matching this
// package's money.Amount representation exactly -- USDT-TRC20 uses 6
// decimals, the same precision money.Amount already carries, so no
// scaling happens here.
func buildCalldata(recipientBase58 string, amount money.Amount) ([]byte, error) {
	return abi.Pack(transferMethod, []abi.Param{
		{"address": recipientBase58},
		{"uint256": strconv.FormatInt(int64(amount), 10)},
	})
}

// Digest is SHA256 of unsignedTx (BuildTransfer's own return value) --
// both this transaction's own txID, TRON's convention for every
// transaction on the chain, and the exact 32 bytes internal/signing's
// SigningService.Sign is asked to sign. Confirmed live: SHA256 of a real
// triggersmartcontract response's raw_data_hex matched that response's
// own txID field, byte for byte.
func Digest(unsignedTx []byte) [32]byte {
	return sha256.Sum256(unsignedTx)
}
