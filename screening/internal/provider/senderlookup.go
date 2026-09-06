package provider

import (
	"context"
	"fmt"
	"sync"
)

// FakeSenderAddressLookup is the only SenderAddressLookup implementation
// until C1 ships sender_address and C3.3 adds the real HTTP-backed one.
// It returns a configured address per external_id, and a typed error for
// anything not configured -- never a zero-value address masquerading as
// a real lookup result.
type FakeSenderAddressLookup struct {
	mu        sync.RWMutex
	addresses map[string]string
}

// NewFakeSenderAddressLookup returns an empty fake. Configure it with Set.
func NewFakeSenderAddressLookup() *FakeSenderAddressLookup {
	return &FakeSenderAddressLookup{addresses: make(map[string]string)}
}

// Set configures externalID to resolve to address.
func (f *FakeSenderAddressLookup) Set(externalID, address string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addresses[externalID] = address
}

// GetSenderAddress implements SenderAddressLookup.
func (f *FakeSenderAddressLookup) GetSenderAddress(ctx context.Context, externalID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	address, ok := f.addresses[externalID]
	if !ok {
		return "", fmt.Errorf("fake sender address lookup: no address configured for external_id %q", externalID)
	}
	return address, nil
}
