// Package replay is C2.10: the deterministic harness that drives C2's
// whole pipeline (ingestion, candidate detection, finality, reporting to
// a real C1) through the scenario mix the build spec names, and is the
// acceptance gate for C2 as a whole.
package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

// simNode is one controllable, fake BSC RPC provider: a real JSON-RPC 2.0
// HTTP server, dialed with the real ethclient, so the harness exercises
// genuine wire-level round trips through chain.Pool, not a hand-rolled
// interface mock -- same posture as every fake node elsewhere in this
// module's own tests.
//
// This is deliberately NOT a real EVM. go-ethereum's own
// ethclient/simulated.Backend (a real embedded EVM, used successfully in
// C2.4's classify_test.go for exactly this reason) does not build under
// this environment's Go toolchain for this chunk's purposes -- a
// transitive dependency (cockroachdb/swiss, pulled in by simulated's full
// node stack) uses a runtime linkname pinned to a Go version range that
// does not yet include this environment's compiler. simNode is a
// controllable stand-in for chain STATE (headers, logs, the finalized
// tag) -- everything C2's own logic ever reads from a provider -- which
// is what this chunk's scenarios actually need to control. Parsing and
// classification correctness against genuine EVM-executed bytecode is
// already covered, separately, by C2.4's own tests.
type simNode struct {
	mu   sync.Mutex
	name string

	dark bool // true: every RPC call fails -- "a provider going dark"

	tip          uint64
	blocks       map[uint64]types.Header
	logsByHeight map[uint64][]types.Log

	finalizedHeight uint64
	finalizedHash   common.Hash
}

func newSimNode(name string) *simNode {
	return &simNode{name: name, blocks: make(map[uint64]types.Header), logsByHeight: make(map[uint64][]types.Log)}
}

func (n *simNode) client() (*ethclient.Client, *httptest.Server) {
	srv := httptest.NewServer(http.HandlerFunc(n.handle))
	c, err := ethclient.DialContext(context.Background(), srv.URL)
	if err != nil {
		panic(fmt.Sprintf("dialing sim node %s: %v", n.name, err))
	}
	return c, srv
}

// setDark simulates a provider going entirely offline: every RPC call
// this node receives while dark returns an error, exactly what a real
// network partition or a crashed node looks like from chain.Pool's side.
func (n *simNode) setDark(dark bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.dark = dark
}

// commitBlock records height's header (parent hash computed from
// whatever this node currently has at height-1, or the zero hash if it
// has nothing there -- a genuinely absent parent, not a lie) and its
// Transfer logs, advancing tip if height is now the furthest this node
// has seen. Calling this again for a height it already has -- with
// DIFFERENT logs or timestamp -- is how a scenario simulates history
// being rewritten: nothing here treats a height as immutable once set.
func (n *simNode) commitBlock(height uint64, at time.Time, logs []types.Log) common.Hash {
	n.mu.Lock()
	defer n.mu.Unlock()

	var parentHash common.Hash
	if parent, ok := n.blocks[height-1]; ok {
		parentHash = parent.Hash()
	}
	header := types.Header{
		ParentHash: parentHash,
		Number:     new(big.Int).SetUint64(height),
		Difficulty: big.NewInt(0),
		GasLimit:   30_000_000,
		Time:       uint64(at.Unix()),
		Extra:      []byte{},
	}
	n.blocks[height] = header
	if height > n.tip {
		n.tip = height
	}
	for i := range logs {
		logs[i].BlockNumber = height
	}
	n.logsByHeight[height] = logs
	return header.Hash()
}

// setFinalized advances this node's own finalized tag to height, hashed
// against whatever header it currently has there. Each node's finalized
// tag is independent -- exactly what makes injecting provider
// disagreement (different nodes finalizing different heights, or the
// same height with different hashes) possible.
func (n *simNode) setFinalized(height uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.finalizedHeight = height
	if h, ok := n.blocks[height]; ok {
		n.finalizedHash = h.Hash()
	}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (n *simNode) handle(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	n.mu.Lock()
	dark := n.dark
	n.mu.Unlock()
	if dark {
		writeRPCError(w, req.ID, fmt.Sprintf("simNode %s: offline", n.name))
		return
	}

	switch req.Method {
	case "eth_getBlockByNumber":
		n.handleGetBlockByNumber(w, req)
	case "eth_getLogs":
		n.handleGetLogs(w, req)
	case "eth_chainId":
		writeRPCResult(w, req.ID, "0x38")
	default:
		writeRPCError(w, req.ID, fmt.Sprintf("simNode %s: unsupported method %q", n.name, req.Method))
	}
}

// handleGetBlockByNumber branches exactly like C2.2/C2.3's own fake
// nodes: "finalized" serves the minimal {number,hash} shape the raw
// CallContext-based finality lookup parses; "latest" and a specific
// height serve a full header for ethclient's typed HeaderByNumber calls.
func (n *simNode) handleGetBlockByNumber(w http.ResponseWriter, req rpcRequest) {
	var args []json.RawMessage
	if err := json.Unmarshal(req.Params, &args); err != nil || len(args) == 0 {
		writeRPCError(w, req.ID, "malformed eth_getBlockByNumber params")
		return
	}
	var tag string
	if err := json.Unmarshal(args[0], &tag); err != nil {
		writeRPCError(w, req.ID, "malformed block tag")
		return
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	if tag == "finalized" {
		writeRPCResult(w, req.ID, map[string]any{
			"number": fmt.Sprintf("0x%x", n.finalizedHeight),
			"hash":   n.finalizedHash.Hex(),
		})
		return
	}

	height := n.tip
	if tag != "latest" {
		h, err := hexToUint64(tag)
		if err != nil {
			writeRPCError(w, req.ID, fmt.Sprintf("bad block number %q: %v", tag, err))
			return
		}
		height = h
	}
	header, ok := n.blocks[height]
	if !ok {
		writeRPCResult(w, req.ID, nil)
		return
	}
	writeRPCResult(w, req.ID, header)
}

// handleGetLogs performs REAL fromBlock/toBlock range filtering against
// this node's own logsByHeight -- unlike the smaller per-package fake
// nodes elsewhere in this module (which just return one fixed slice
// regardless of the query), this harness's scenarios genuinely rely on a
// range-scoped response, since candidates.ScanRange is called against
// specific, evolving block ranges as ingestion advances.
func (n *simNode) handleGetLogs(w http.ResponseWriter, req rpcRequest) {
	var args []struct {
		FromBlock string `json:"fromBlock"`
		ToBlock   string `json:"toBlock"`
	}
	if err := json.Unmarshal(req.Params, &args); err != nil || len(args) == 0 {
		writeRPCError(w, req.ID, "malformed eth_getLogs params")
		return
	}
	from, err := hexToUint64(args[0].FromBlock)
	if err != nil {
		writeRPCError(w, req.ID, "malformed fromBlock")
		return
	}
	to, err := hexToUint64(args[0].ToBlock)
	if err != nil {
		writeRPCError(w, req.ID, "malformed toBlock")
		return
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	var out []types.Log
	for h := from; h <= to; h++ {
		out = append(out, n.logsByHeight[h]...)
	}
	writeRPCResult(w, req.ID, out)
}

func hexToUint64(s string) (uint64, error) {
	var v uint64
	_, err := fmt.Sscanf(s, "0x%x", &v)
	return v, err
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	body, err := json.Marshal(result)
	if err != nil {
		panic(err)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": json.RawMessage(body)})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": -32000, "message": message},
	})
}
