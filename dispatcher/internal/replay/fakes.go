package replay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"google.golang.org/protobuf/proto"

	"dispatcher/internal/energy"
)

// fakeChain stands in for a real TRON node's broadcast and finality
// surfaces -- the chain itself, which this harness cannot assume is
// reachable any more than C4's own did (see this package's own Config
// doc comment). Deterministic txids (SHA256 of raw_data, TRON's own real
// convention, verified live while building C5.4), and forceable finality
// per txid so a scenario controls exactly when ConfirmFinality's own
// poll succeeds.
type fakeChain struct {
	mu       sync.Mutex
	calls    int
	final    map[string]bool
	failNext bool
}

func newFakeChain() *fakeChain { return &fakeChain{final: make(map[string]bool)} }

func (c *fakeChain) Broadcast(ctx context.Context, tx *core.Transaction) (string, error) {
	c.mu.Lock()
	c.calls++
	fail := c.failNext
	c.failNext = false
	c.mu.Unlock()
	if fail {
		return "", errors.New("fakeChain: forced broadcast failure")
	}
	raw, err := proto.Marshal(tx.RawData)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (c *fakeChain) IsFinal(ctx context.Context, tronTxID string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.final[tronTxID], nil
}

func (c *fakeChain) SetFinal(txid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.final[txid] = true
}

func (c *fakeChain) ForceNextBroadcastFailure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failNext = true
}

func (c *fakeChain) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// fakeEnergyReserver stands in for C4's own reservation client -- C4.8's
// HTTP boundary does not exist yet in any run this harness can assume
// (see "Read this second" in the build spec), so this is what every
// scenario reserves energy against. Forceable per-call outcome so a
// scenario can inject FAILED (zero C1 impact) and a client-side timeout
// (distinct from FAILED) independently.
type fakeEnergyReserver struct {
	mu           sync.Mutex
	calls        []int64
	forceFail    bool
	forceTimeout bool
	seq          int64
}

func newFakeEnergyReserver() *fakeEnergyReserver { return &fakeEnergyReserver{} }

func (f *fakeEnergyReserver) Reserve(ctx context.Context, externalID, targetAddress string, units int64, tier string, deadline time.Time, idempotencyKey string) (energy.Reservation, error) {
	f.mu.Lock()
	f.calls = append(f.calls, units)
	fail, timeout := f.forceFail, f.forceTimeout
	f.seq++
	seq := f.seq
	f.mu.Unlock()

	if timeout {
		return energy.Reservation{}, fmt.Errorf("fakeEnergyReserver: forced client-side timeout (distinct from FAILED)")
	}
	if fail {
		return energy.Reservation{ID: seq, Status: "FAILED"}, nil
	}
	return energy.Reservation{ID: seq, Status: "CONFIRMED"}, nil
}

func (f *fakeEnergyReserver) ForceFail()    { f.mu.Lock(); f.forceFail = true; f.mu.Unlock() }
func (f *fakeEnergyReserver) ForceTimeout() { f.mu.Lock(); f.forceTimeout = true; f.mu.Unlock() }

func (f *fakeEnergyReserver) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}
