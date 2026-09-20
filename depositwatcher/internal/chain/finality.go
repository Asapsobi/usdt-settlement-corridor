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

// blockNumberHashRPC is the subset of eth_getBlockByNumber's response
// this package needs, whether the request asked for the "finalized" tag
// or a specific numeric height.
type blockNumberHashRPC struct {
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
// minAgreement configured providers independently confirm as the
// chain's current "finalized" block, per BSC's BEP-126 fast-finality
// consensus (see the C2 build spec's "Read this second" section for why
// this is the primary finality signal here, not a fixed confirmation
// depth).
//
// BSC's finalized pointer advances roughly once a block (well under two
// seconds); two independent providers queried even milliseconds apart
// routinely report different heights despite both being healthy and on
// the same canonical chain. Requiring their self-reported (height,
// hash) pairs to match exactly would therefore treat healthy providers
// as disagreeing far more often than a real fork ever would. Instead,
// every provider within finalityTolerance blocks of the highest report
// is normalized onto one reference height -- the lowest height among
// that group, since a provider reporting a HIGHER finalized height has,
// by definition, already finalized every ancestor of that block too,
// including the one at the reference height -- and providers above the
// reference height are asked (one more RPC call each) what hash THEY
// hold at that exact height. Only after every in-tolerance provider is
// talking about the literal same height does this compare hashes, so
// the actual safety property (never resolve a genuine disagreement by
// picking a side) is unchanged: a provider more than finalityTolerance
// blocks behind is excluded outright, same as a timeout or an error,
// and two providers that genuinely disagree at the reference height
// still hard-fail exactly as before.
func (p *Pool) LatestFinalized(ctx context.Context) (height uint64, agreedHash common.Hash, err error) {
	results := p.queryFinalizedFromAllProviders(ctx)

	successes := make([]providerResult, 0, len(results))
	for _, r := range results {
		if r.err != nil {
			p.recordFailure(r.name, r.err)
			continue
		}
		successes = append(successes, r)
	}

	var refHeight uint64
	inTolerance := make([]providerResult, 0, len(successes))
	if len(successes) > 0 {
		maxHeight := successes[0].height
		for _, r := range successes[1:] {
			if r.height > maxHeight {
				maxHeight = r.height
			}
		}
		refHeight = maxHeight
		for _, r := range successes {
			if r.height+p.finalityTolerance < maxHeight {
				p.recordFailure(r.name, fmt.Errorf("reported finalized height %d, more than %d blocks behind the group's %d -- excluded, not used to lower the reference point",
					r.height, p.finalityTolerance, maxHeight))
				continue
			}
			if r.height < refHeight {
				refHeight = r.height
			}
			inTolerance = append(inTolerance, r)
		}
	}

	// Resolve every in-tolerance provider's hash AT refHeight specifically
	// -- reusing what it already reported if that was refHeight, or
	// asking it directly otherwise (safe: refHeight is at or behind that
	// provider's own already-finalized point, never a prediction).
	groups := make(map[common.Hash][]string) // hash at refHeight -> provider names reporting it
	for _, r := range inTolerance {
		h := r.hash
		if r.height != refHeight {
			var normErr error
			h, normErr = queryBlockHashAt(ctx, p.providerNamed(r.name), refHeight)
			if normErr != nil {
				p.recordFailure(r.name, fmt.Errorf("resolving hash at reference height %d: %w", refHeight, normErr))
				continue
			}
		}
		groups[h] = append(groups[h], r.name)
	}

	// Find the largest group that reaches the agreement threshold, and
	// detect a tie at the top among groups that do.
	var winnerHash common.Hash
	winnerCount := 0
	ambiguous := false
	for h, names := range groups {
		if len(names) < p.minAgreement {
			continue // doesn't reach the threshold at all -- not a contender
		}
		switch {
		case winnerCount == 0:
			winnerHash, winnerCount = h, len(names)
		case len(names) == winnerCount:
			ambiguous = true
		case len(names) > winnerCount:
			// A strictly larger group supersedes an earlier smaller one
			// that had also crossed the threshold -- not itself
			// ambiguous, since one group is unambiguously the largest.
			winnerHash, winnerCount = h, len(names)
			ambiguous = false
		}
	}

	// A successfully-normalized provider is only penalized for THIS round
	// if there was a genuine disagreement to be on the wrong side of --
	// i.e., at least two DISTINCT hashes came back at refHeight among the
	// providers that got that far (len(groups) > 1). A lone group whose
	// members all agree with each other is never penalized merely for
	// falling short of minAgreement because other providers errored,
	// timed out, or were excluded as too far behind: those providers'
	// own failures were already recorded above, and the survivors did
	// nothing wrong -- marking them down too would make a perfectly
	// healthy provider look unhealthy purely because a DIFFERENT
	// provider went down, exactly the misleading signal an operator
	// chasing a health dashboard must not see.
	switch {
	case len(groups) > 1:
		winningNames := make(map[string]bool)
		if winnerCount > 0 && !ambiguous {
			for _, n := range groups[winnerHash] {
				winningNames[n] = true
			}
		}
		for h, names := range groups {
			for _, n := range names {
				if winningNames[n] {
					p.recordSuccess(n)
					continue
				}
				p.recordFailure(n, fmt.Errorf("reported hash %s at reference height %d, which was not this round's agreed result",
					h, refHeight))
			}
		}
	case winnerCount > 0:
		// Exactly one distinct answer, and it reached the threshold: no
		// disagreement occurred, every respondent succeeded.
		for _, n := range groups[winnerHash] {
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
	return refHeight, winnerHash, nil
}

// providerNamed returns the configured Provider with the given name --
// providerResult only carries a name, not the Provider itself, so
// resolving a hash at a specific height needs this lookup back into the
// pool's own provider list.
func (p *Pool) providerNamed(name string) Provider {
	for _, prov := range p.providers {
		if prov.Name == name {
			return prov
		}
	}
	panic("chain: providerNamed called with an unconfigured name " + name) // unreachable: names come from p.providers itself
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
	var result blockNumberHashRPC
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

// queryBlockHashAt returns the hash prov holds for the block at the
// given numeric height -- used by LatestFinalized to normalize a
// provider that reported a higher finalized height onto a shared,
// lower reference height. height is always at or behind that
// provider's own already-finalized point when this is called, so this
// is a historical lookup, never a prediction of a not-yet-final block.
func queryBlockHashAt(ctx context.Context, prov Provider, height uint64) (common.Hash, error) {
	var result blockNumberHashRPC
	err := prov.Client.Client().CallContext(ctx, &result, "eth_getBlockByNumber", fmt.Sprintf("0x%x", height), false)
	if err != nil {
		return common.Hash{}, fmt.Errorf("%s: eth_getBlockByNumber(0x%x): %w", prov.Name, height, err)
	}
	if result.Hash == "" {
		return common.Hash{}, fmt.Errorf("%s: eth_getBlockByNumber(0x%x) returned no hash -- this provider "+
			"may not have block %d yet, which should be unreachable since it already reported a higher finalized height",
			prov.Name, height, height)
	}
	return common.HexToHash(result.Hash), nil
}

func hexToUint64(s string) (uint64, error) {
	return strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
}
