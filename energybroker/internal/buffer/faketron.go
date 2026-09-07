package buffer

import (
	"context"
	"sync"
)

// FakeTronReader is a deterministic, controllable TronEnergyReader: the
// only implementation this chunk ships (see TronEnergyReader's own doc
// comment for why a real client is a later, separate concern). Keyed by
// (address, delegationID) so a test can simulate a specific delegation
// being fully present, partially reduced, or fully revoked, independent
// of every other delegation to the same address.
type FakeTronReader struct {
	mu    sync.Mutex
	units map[string]int64
	errs  map[string]error
	auto  int64
}

// NewFakeTronReader returns a FakeTronReader with no delegations known
// yet -- DelegationUnits returns 0 (never an error) for anything not
// explicitly set via SetUnits, matching a real chain's own "nothing
// delegated" answer for an address/delegation it has never seen. Call
// AutoConfirm to change that default for tests that exercise a whole
// Buffer (where a fresh delegation's own id is generated dynamically
// and not knowable in advance of the call that produces it).
func NewFakeTronReader() *FakeTronReader {
	return &FakeTronReader{units: make(map[string]int64), errs: make(map[string]error)}
}

// AutoConfirm changes the default for any (address, delegationID) this
// reader has no explicit SetUnits entry for, from 0 to units -- for
// tests where a fresh delegation's own id is generated dynamically by
// the provider under test and so cannot be pre-configured via SetUnits.
// A test that still needs to simulate one SPECIFIC delegation being
// absent or partial can call SetUnits for that one id after enabling
// AutoConfirm; SetUnits always takes precedence over the auto-confirm
// default for the key it names.
func (f *FakeTronReader) AutoConfirm(units int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auto = units
}

func key(address, delegationID string) string { return address + "\x00" + delegationID }

// SetUnits configures address/delegationID to report units currently
// delegated on-chain -- 0 simulates a fully revoked/expired delegation,
// a value below what was originally requested simulates a partial
// reduction, and a value at or above the original amount simulates it
// still being fully present.
func (f *FakeTronReader) SetUnits(address, delegationID string, units int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.errs, key(address, delegationID))
	f.units[key(address, delegationID)] = units
}

// SetError configures address/delegationID's next queries to fail with
// err, simulating a chain-read failure distinct from a real "0 units"
// answer.
func (f *FakeTronReader) SetError(address, delegationID string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[key(address, delegationID)] = err
}

// DelegationUnits implements TronEnergyReader.
func (f *FakeTronReader) DelegationUnits(ctx context.Context, address, delegationID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(address, delegationID)
	if err, ok := f.errs[k]; ok {
		return 0, err
	}
	if units, ok := f.units[k]; ok {
		return units, nil
	}
	return f.auto, nil
}
