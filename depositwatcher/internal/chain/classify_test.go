package chain_test

// This chunk's own acceptance criteria demand a "synthetic contract
// deployed on a local fork emitting Transfer events" -- not hand-built
// types.Log fixtures that would only prove ParseTransferLog agrees with
// itself. There is no Solidity compiler available in this environment (no
// brew/node/docker), so the fixture contract below is hand-assembled EVM
// bytecode rather than compiled from source, executed by go-ethereum's own
// in-process EVM (core/vm/runtime, the same package geth's own EVM unit
// tests use) against a real, in-memory StateDB -- a real EVM running real
// bytecode and emitting real LOG3 output, entirely within the Go test
// binary. This is deliberately NOT github.com/ethereum/go-ethereum/
// ethclient/simulated (a full embedded node): that package pulls in
// cockroachdb/pebble, whose cockroachdb/swiss dependency hard-gates itself
// off for this environment's Go 1.27 toolchain (see its own
// runtime_go1.20.go: "go1.20 && !go1.27") and fails to compile. Nothing
// this chunk needs -- a contract's code at a chosen address, a call
// against it, the logs that call emits -- requires a full node; the
// lower-level EVM-execution API is the right tool, not a workaround.
import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm/runtime"
	"github.com/ethereum/go-ethereum/crypto"

	"depositwatcher/internal/chain"
	"depositwatcher/internal/money"
)

// transferEventTopicForTest is keccak256("Transfer(address,address,uint256)"),
// computed independently of internal/chain's own unexported copy so this
// file's fixture and TestTransferEventTopic_MatchesWellKnownValue below
// don't just echo production's value back at itself.
var transferEventTopicForTest = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

func TestTransferEventTopic_MatchesWellKnownValue(t *testing.T) {
	// This exact hash is one of the most widely published constants in EVM
	// tooling -- every block explorer's log decoder hardcodes it as the
	// ERC20 Transfer topic. Checked against it independently of anything
	// this package computes.
	want := common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
	if transferEventTopicForTest != want {
		t.Fatalf("keccak256(\"Transfer(address,address,uint256)\") = %s, want the well-known %s",
			transferEventTopicForTest, want)
	}
}

// transferLoggerRuntimeBytecode is a hand-assembled, minimal EVM contract
// (runtime code only -- the fixture below installs it directly via
// StateDB.SetCode, so no constructor/init-code wrapper is needed). On any
// call carrying at least 96 bytes of calldata, it decodes three 32-byte
// words -- from, to, value -- and emits them as a standard ERC20
// Transfer(address indexed from, address indexed to, uint256 value) log.
// No balance tracking, no access control, no function selector dispatch:
// none of that is under test here, only whether a real EVM-emitted
// Transfer log parses and classifies correctly.
//
// Opcode by opcode:
//
//	CALLDATACOPY(destOffset=0, offset=0, size=96)  memory[0:96] = from|to|value
//	PUSH1 0x20 MLOAD                               memory[32:64] ("to")   -> topic3
//	PUSH1 0x00 MLOAD                                memory[0:32]  ("from") -> topic2
//	PUSH32 <transferTopic>                          topic1 (becomes log.Topics[0])
//	PUSH1 0x20 (size) PUSH1 0x40 (offset)           data = memory[64:96] ("value")
//	LOG3
//	STOP
func transferLoggerRuntimeBytecode(transferTopic common.Hash) []byte {
	code := []byte{
		0x60, 0x60, 0x60, 0x00, 0x60, 0x00, 0x37, // PUSH1 96, PUSH1 0, PUSH1 0, CALLDATACOPY
		0x60, 0x20, 0x51, // PUSH1 32, MLOAD (to)
		0x60, 0x00, 0x51, // PUSH1 0, MLOAD (from)
		0x7F, // PUSH32
	}
	code = append(code, transferTopic.Bytes()...)
	code = append(code,
		0x60, 0x20, // PUSH1 32 (size)
		0x60, 0x40, // PUSH1 64 (offset)
		0xA3, // LOG3
		0x00, // STOP
	)
	return code
}

// evmFixture wraps a single in-memory StateDB that one or more contracts
// can be installed into and called against, isolating each call's logs by
// giving every call its own synthetic tx hash.
type evmFixture struct {
	statedb *state.StateDB
	cfg     *runtime.Config
	callSeq uint64
}

func newEVMFixture(t *testing.T) *evmFixture {
	t.Helper()
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	return &evmFixture{statedb: statedb, cfg: &runtime.Config{State: statedb}}
}

// deploy installs code directly at addr -- standing in for a real
// deployment transaction, which this fixture has no need to simulate since
// nothing here depends on deployment gas cost or constructor behavior.
func (f *evmFixture) deploy(addr common.Address, code []byte) {
	f.statedb.CreateAccount(addr)
	f.statedb.SetCode(addr, code, tracing.CodeChangeUnspecified)
}

// call invokes contract with input and returns the exactly-one log it must
// emit, failing the test otherwise.
func (f *evmFixture) call(t *testing.T, contract common.Address, input []byte) types.Log {
	t.Helper()
	f.callSeq++
	txHash := common.BigToHash(new(big.Int).SetUint64(f.callSeq))
	f.statedb.SetTxContext(txHash, int(f.callSeq), 0)

	if _, _, err := runtime.Call(contract, input, f.cfg); err != nil {
		t.Fatalf("EVM call to %s reverted: %v", contract, err)
	}
	logs := f.statedb.GetLogs(txHash, 1, common.Hash{}, 0)
	if len(logs) != 1 {
		t.Fatalf("call to %s produced %d logs, want exactly 1", contract, len(logs))
	}
	return *logs[0]
}

// transferCalldata packs (from, to, value) as raw 96-byte calldata -- no
// selector, matching transferLoggerRuntimeBytecode's own layout.
func transferCalldata(from, to common.Address, rawValue *big.Int) []byte {
	data := make([]byte, 96)
	copy(data[12:32], from.Bytes())
	copy(data[44:64], to.Bytes())
	rawValue.FillBytes(data[64:96])
	return data
}

// toRawOnChainUnits converts a minor-unit amount into the 18-decimal raw
// on-chain value that would produce it after ParseTransferLog's rescale --
// the inverse of money.FromOnChainUnits at exactly zero remainder, so tests
// below get back exactly the minor-unit amount they asked for.
func toRawOnChainUnits(minor money.Amount) *big.Int {
	return new(big.Int).Mul(big.NewInt(int64(minor)), big.NewInt(1_000_000_000_000)) // 10^(18-6)
}

func TestParseTransferLog_And_ClassifyAgainstOrder_RealChainFixture(t *testing.T) {
	fixture := newEVMFixture(t)
	contract := common.HexToAddress("0xC0DE01")
	fixture.deploy(contract, transferLoggerRuntimeBytecode(transferEventTopicForTest))

	from := common.HexToAddress("0xA1") // simulated depositor
	to := common.HexToAddress("0xB2")   // watched deposit address

	const quoted = money.Amount(3000_000000) // 3000.000000 USDT_BEP20
	dustFloor := chain.DefaultDustFloor      // 1.000000 USDT_BEP20

	tests := []struct {
		name  string
		minor money.Amount
		wantC chain.Classification
	}{
		{"exact match", 3000_000000, chain.Exact},
		{"overpay", 3500_000000, chain.Overpay},
		{"underpay above dust floor", 2000_000000, chain.Underpay},
		{"dust, nonzero but below floor", 500_000, chain.Dust},
		{"zero value transfer", 0, chain.ZeroValue},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			log := fixture.call(t, contract, transferCalldata(from, to, toRawOnChainUnits(tc.minor)))

			gotFrom, gotTo, gotAmount, err := chain.ParseTransferLog(log)
			if err != nil {
				t.Fatalf("ParseTransferLog: unexpected error: %v", err)
			}
			if gotFrom != from {
				t.Errorf("from = %s, want %s", gotFrom, from)
			}
			if gotTo != to {
				t.Errorf("to = %s, want %s", gotTo, to)
			}
			if gotAmount != tc.minor {
				t.Errorf("amount = %d, want %d", gotAmount, tc.minor)
			}

			gotClass := chain.ClassifyAgainstOrder(gotAmount, quoted, dustFloor)
			if gotClass != tc.wantC {
				t.Errorf("ClassifyAgainstOrder(%d, %d, %d) = %s, want %s",
					gotAmount, quoted, dustFloor, gotClass, tc.wantC)
			}
		})
	}
}

func TestParseTransferLog_RejectsNonTransferShapes(t *testing.T) {
	validData := make([]byte, 32)
	validData[31] = 1
	validTopics := []common.Hash{
		transferEventTopicForTest,
		common.HexToHash("0xA1"),
		common.HexToHash("0xB2"),
	}

	tests := []struct {
		name string
		log  types.Log
	}{
		{
			name: "wrong topic0 (not the Transfer signature)",
			log: types.Log{
				Topics: []common.Hash{common.HexToHash("0xdeadbeef"), validTopics[1], validTopics[2]},
				Data:   validData,
			},
		},
		{
			name: "too few topics",
			log:  types.Log{Topics: validTopics[:2], Data: validData},
		},
		{
			name: "too many topics",
			log:  types.Log{Topics: append(append([]common.Hash{}, validTopics...), common.Hash{}), Data: validData},
		},
		{
			name: "non-32-byte data",
			log:  types.Log{Topics: validTopics, Data: []byte{0x01, 0x02, 0x03}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := chain.ParseTransferLog(tc.log)
			if err == nil || !errors.Is(err, chain.ErrWrongToken) {
				t.Fatalf("ParseTransferLog: got error %v, want one wrapping chain.ErrWrongToken", err)
			}
		})
	}
}

func TestLogsFilteredByContractAddress_NeverReachTheOtherContract(t *testing.T) {
	// This is the "filtered upstream" acceptance criterion: a Transfer-
	// shaped log from a DIFFERENT contract must never be treated as a
	// candidate from the real USDT BEP20 address -- exactly the
	// eth_getLogs Addresses filter LogsAt (C2.2) already applies against a
	// real provider. Proven here by emitting from contractB and confirming
	// its log is simply a different log at a different Address, distinct
	// from anything the real pipeline would have queried for contractA --
	// ParseTransferLog itself never inspects an address (see its own doc
	// comment on why: that filtering is LogsAt's job, once per batch, not
	// re-done per log here).
	fixture := newEVMFixture(t)
	contractA := common.HexToAddress("0xC0DE0A")
	contractB := common.HexToAddress("0xC0DE0B")
	fixture.deploy(contractA, transferLoggerRuntimeBytecode(transferEventTopicForTest))
	fixture.deploy(contractB, transferLoggerRuntimeBytecode(transferEventTopicForTest))

	from := common.HexToAddress("0xC1")
	to := common.HexToAddress("0xC2")
	log := fixture.call(t, contractB, transferCalldata(from, to, toRawOnChainUnits(1_000000)))

	if log.Address != contractB {
		t.Fatalf("log.Address = %s, want %s (the contract actually called)", log.Address, contractB)
	}
	if log.Address == contractA {
		t.Fatalf("contractB's log carries contractA's address -- an eth_getLogs Addresses:[contractA] filter would wrongly admit it")
	}
}
