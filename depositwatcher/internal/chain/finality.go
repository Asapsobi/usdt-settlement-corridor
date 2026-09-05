package chain

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/common"
)

var (
	// ErrNoAgreement means fewer than minAgreement providers reported the
	// same (height, hash) this round -- whether because they disagreed,
	// errored, or timed out. A hard error, never a degraded success on
	// whatever happened to be the largest group.
	ErrNoAgreement = errors.New("chain: fewer than the required number of providers agree on the finalized block")

	// ErrAmbiguousAgreement means two or more DIFFERENT (height, hash)
	// pairs each independently reached minAgreement -- an even split
	// across a genuine network disagreement. Picking either one over the
	// other would be exactly the "silently resolved by trusting one
	// side" behavior this package exists to refuse.
	ErrAmbiguousAgreement = errors.New("chain: multiple groups of providers reached agreement on DIFFERENT finalized blocks")
)

// finalizedBlockRPC is the subset of eth_getBlockByNumber's response this
// package needs.
type finalizedBlockRPC struct {
	Number string `json:"number"`
	Hash   string `json:"hash"`
}

type providerResult struct {
	name   string
	height uint64
	hash   common.Hash
	err    error
}

// LatestFinalized returns the block height and hash that at least
// minAgreement configured providers independently report as the chain's
// current "finalized" block, per BSC's BEP-126 fast-finality consensus
// (see the C2 build spec's "Read this second" section for why this is
// the primary finality signal here, not a fixed confirmation depth).
//
// A provider that times out, errors, or reports a different (height,
// hash) than the agreeing group is excluded from the agreement count --
// never silently counted as agreeing, and never used as a tie-breaker.
func (p *Pool) LatestFinalized(ctx context.Context) (height uint64, agreedHash common.Hash, err error) {
	results := p.queryFinalizedFromAllProviders(ctx)

	type key struct {
		height uint64
		hash   common.Hash
	}
	groups := make(map[key][]string) // key -> provider names reporting it
	for _, r := range results {
		if r.err != nil {
			p.recordFailure(r.name, r.err)
			continue
		}
		k := key{r.height, r.hash}
		groups[k] = append(groups[k], r.name)
	}

	// Find the largest group that reaches the agreement threshold, and
	// detect a tie at the top among groups that do.
	var winner key
	winnerCount := 0
	ambiguous := false
	for k, names := range groups {
		if len(names) < p.minAgreement {
			continue // doesn't reach the threshold at all -- not a contender
		}
		switch {
		case winnerCount == 0:
			winner, winnerCount = k, len(names)
		case len(names) == winnerCount:
			ambiguous = true
		case len(names) > winnerCount:
			// A strictly larger group supersedes an earlier smaller one
			// that had also crossed the threshold -- not itself
			// ambiguous, since one group is unambiguously the largest.
			winner, winnerCount = k, len(names)
			ambiguous = false
		}
	}

	// A successfully-responding provider is only penalized for THIS round
	// if there was a genuine disagreement to be on the wrong side of --
	// i.e., at least two DISTINCT answers came back among the providers
	// that responded at all (len(groups) > 1). A lone group whose members
	// all agree with each other is never penalized merely for falling
	// short of minAgreement because other providers errored: those
	// providers' own failures were already recorded in the loop above,
	// and the survivors did nothing wrong -- marking them down too would
	// make a perfectly healthy provider look unhealthy purely because a
	// DIFFERENT provider went down, exactly the misleading signal an
	// operator chasing a health dashboard must not see.
	switch {
	case len(groups) > 1:
		winningNames := make(map[string]bool)
		if winnerCount > 0 && !ambiguous {
			for _, n := range groups[winner] {
				winningNames[n] = true
			}
		}
		for k, names := range groups {
			for _, n := range names {
				if winningNames[n] {
					p.recordSuccess(n)
					continue
				}
				p.recordFailure(n, fmt.Errorf("reported (height=%d hash=%s), which was not this round's agreed result",
					k.height, k.hash))
			}
		}
	case winnerCount > 0:
		// Exactly one distinct answer, and it reached the threshold: no
		// disagreement occurred, every respondent succeeded.
		for _, n := range groups[winner] {
			p.recordSuccess(n)
		}
		// The remaining case -- exactly one group, below threshold -- has
		// nothing further to record here: its members agreed with each
		// other and simply didn't have enough company this round, which
		// is not a fault of theirs to be penalized for.
	}

	if ambiguous {
		return 0, common.Hash{}, fmt.Errorf("%w: need >= %d providers to agree, but multiple different blocks each had that many",
			ErrAmbiguousAgreement, p.minAgreement)
	}
	if winnerCount == 0 {
		return 0, common.Hash{}, fmt.Errorf("%w: need %d, best group had fewer (of %d providers queried)",
			ErrNoAgreement, p.minAgreement, len(p.providers))
	}
	return winner.height, winner.hash, nil
}

func (p *Pool) queryFinalizedFromAllProviders(ctx context.Context) []providerResult {
	results := make([]providerResult, len(p.providers))
	var wg sync.WaitGroup
	for i, prov := range p.providers {
		wg.Add(1)
		go func(i int, prov Provider) {
			defer wg.Done()
			height, hash, err := queryFinalized(ctx, prov)
			results[i] = providerResult{name: prov.Name, height: height, hash: hash, err: err}
		}(i, prov)
	}
	wg.Wait()
	return results
}

// queryFinalized calls eth_getBlockByNumber("finalized", false) via the
// raw RPC client rather than a typed ethclient helper. The "finalized"
// block tag is a string sentinel per the post-Merge JSON-RPC convention
// BEP-126 reuses -- calling the method by name directly, exactly as this
// chunk's own spec names it, avoids depending on whether ethclient's
// higher-level helpers in a given go-ethereum version accept that
// sentinel the way we'd expect.
func queryFinalized(ctx context.Context, prov Provider) (uint64, common.Hash, error) {
	var result finalizedBlockRPC
	err := prov.Client.Client().CallContext(ctx, &result, "eth_getBlockByNumber", "finalized", false)
	if err != nil {
		return 0, common.Hash{}, fmt.Errorf("%s: eth_getBlockByNumber(finalized): %w", prov.Name, err)
	}
	if result.Number == "" || result.Hash == "" {
		return 0, common.Hash{}, fmt.Errorf("%s: eth_getBlockByNumber(finalized) returned an empty block "+
			"-- this provider may not support the finalized tag", prov.Name)
	}
	height, err := hexToUint64(result.Number)
	if err != nil {
		return 0, common.Hash{}, fmt.Errorf("%s: parsing block number %q: %w", prov.Name, result.Number, err)
	}
	return height, common.HexToHash(result.Hash), nil
}

func hexToUint64(s string) (uint64, error) {
	return strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
}
