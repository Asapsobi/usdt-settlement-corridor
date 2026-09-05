package replay

import (
	"net/http/httptest"
	"time"

	"github.com/ethereum/go-ethereum/core/types"

	"depositwatcher/internal/chain"
)

// simChain wraps three simNodes (invariant 5's minimum of 2, plus one
// spare so "a provider going dark" scenarios still leave enough for
// agreement to remain possible) behind a single real chain.Pool.
type simChain struct {
	nodes []*simNode
	srvs  []*httptest.Server
	pool  *chain.Pool
}

// newSimChain builds a 3-provider simChain with the given agreement
// config, closing every fake HTTP server this creates when t's test
// ends.
func newSimChain(cfg chain.Config) (*simChain, error) {
	names := []string{"A", "B", "C"}
	sc := &simChain{}
	var providers []chain.Provider
	for _, name := range names {
		node := newSimNode(name)
		client, srv := node.client()
		sc.nodes = append(sc.nodes, node)
		sc.srvs = append(sc.srvs, srv)
		providers = append(providers, chain.Provider{Name: name, Client: client})
	}
	pool, err := chain.NewPool(providers, cfg)
	if err != nil {
		sc.Close()
		return nil, err
	}
	sc.pool = pool
	return sc, nil
}

func (sc *simChain) Close() {
	for _, srv := range sc.srvs {
		srv.Close()
	}
}

// commitBlockToAll advances every node's chain by one block at height,
// all agreeing on the same logs and timestamp -- the routine case, no
// disagreement, no reorg.
func (sc *simChain) commitBlockToAll(height uint64, at time.Time, logs []types.Log) {
	for _, n := range sc.nodes {
		// Each node needs its own copy: commitBlock mutates BlockNumber
		// on the slice it's given, and sharing one backing array across
		// nodes would let one node's write clobber another's.
		n.commitBlock(height, at, append([]types.Log(nil), logs...))
	}
}

// rewriteHeight re-commits height on only the given node indices, with
// different logs than whatever they previously agreed on -- simulating a
// chain-history rewrite (a reorg) visible to those specific providers.
// The other nodes are left exactly as they were, so an assertion can
// check what each side of the disagreement independently reports.
func (sc *simChain) rewriteHeight(height uint64, at time.Time, logs []types.Log, nodeIndices ...int) {
	for _, i := range nodeIndices {
		sc.nodes[i].commitBlock(height, at, append([]types.Log(nil), logs...))
	}
}

// finalizeAll advances every node's finalized tag to height in
// agreement.
func (sc *simChain) finalizeAll(height uint64) {
	for _, n := range sc.nodes {
		n.setFinalized(height)
	}
}

// finalizeOne advances a single node's finalized tag independently --
// for injecting disagreement on the finalized-height claim itself.
func (sc *simChain) finalizeOne(index int, height uint64) {
	sc.nodes[index].setFinalized(height)
}

// setDark takes a node fully offline (or restores it).
func (sc *simChain) setDark(index int, dark bool) {
	sc.nodes[index].setDark(dark)
}
