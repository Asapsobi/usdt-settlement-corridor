package dispatch

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fbsobreira/gotron-sdk/pkg/client"
	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"google.golang.org/protobuf/proto"

	"dispatcher/internal/txbuild"
)

// GrpcBroadcastClient implements BroadcastClient against a real TRON
// node's gRPC surface, via gotron-sdk's own GrpcClient -- the same
// library C5.4's txbuild already depends on for construction, reused
// here rather than a second, independently-chosen client.
//
// Unlike every other real client this component builds against (C1, C4,
// S1), this one has NOT been exercised against a live node: doing so
// means actually broadcasting a transaction, which costs a real TRX fee
// and moves real tokens -- not something to do from a build session, and
// not reachable anyway until S1 exists for real (see "Read this first").
// The request/response shapes here come directly from gotron-sdk's own
// Broadcast method (github.com/fbsobreira/gotron-sdk/pkg/client/network.go),
// not guessed, but "the library's own shape is correct" and "this
// specific call has been proven against a real node" are different
// claims -- only the first one is true today.
type GrpcBroadcastClient struct {
	grpc *client.GrpcClient
}

// NewGrpcBroadcastClient connects to a TRON node's gRPC endpoint (e.g.
// "grpc.trongrid.io:50051"). The connection is established once, here,
// and reused for every Broadcast call.
func NewGrpcBroadcastClient(nodeAddress string, timeout time.Duration) (*GrpcBroadcastClient, error) {
	g := client.NewGrpcClientWithTimeout(nodeAddress, timeout)
	if err := g.Start(); err != nil {
		return nil, fmt.Errorf("dispatch: connecting to TRON node %q: %w", nodeAddress, err)
	}
	return &GrpcBroadcastClient{grpc: g}, nil
}

// CurrentBlockReference fetches a fresh txbuild.BlockReference from this
// client's own node -- GetNowBlockCtx, the same real gRPC call every
// TRON wallet uses to pick a recent reference block before building a
// transaction. Timestamp/Expiration come from the wall clock, not the
// block's own timestamp (matching every fake caller in this codebase,
// e.g. the replay harness's own buildTransfer), with a 2-minute
// expiration window -- TRON's own tolerance, per BlockReference's doc
// comment. Like Broadcast itself, this has not been exercised against a
// live node from a build session.
func (c *GrpcBroadcastClient) CurrentBlockReference(ctx context.Context) (txbuild.BlockReference, error) {
	block, err := c.grpc.GetNowBlockCtx(ctx)
	if err != nil {
		return txbuild.BlockReference{}, fmt.Errorf("dispatch: fetching current TRON block: %w", err)
	}
	if len(block.Blockid) != 32 {
		return txbuild.BlockReference{}, fmt.Errorf("dispatch: current TRON block returned a %d-byte blockid, want 32", len(block.Blockid))
	}
	var hash [32]byte
	copy(hash[:], block.Blockid)

	now := time.Now().UTC()
	return txbuild.BlockReference{
		BlockNumber: block.BlockHeader.RawData.Number,
		BlockHash:   hash,
		Timestamp:   now,
		Expiration:  now.Add(2 * time.Minute),
	}, nil
}

// Broadcast submits tx and returns its txID on success. TRON's own
// Broadcast RPC reports failure via Return.Result=false (with a code and
// a human-readable message), not a transport-level error, so a false
// Result is translated into a Go error here rather than silently
// returning an empty txid. The txid itself is never read off the RPC
// response (TRON's Return carries no such field) -- it is, by TRON's own
// convention verified against a live node while building C5.4,
// SHA256(raw_data), computed here directly from tx.RawData exactly as
// txbuild.Digest already does from BuildTransfer's own output.
func (c *GrpcBroadcastClient) Broadcast(ctx context.Context, tx *core.Transaction) (string, error) {
	ret, err := c.grpc.BroadcastCtx(ctx, tx)
	if err != nil {
		return "", fmt.Errorf("dispatch: broadcasting to TRON node: %w", err)
	}
	if !ret.Result {
		return "", fmt.Errorf("dispatch: TRON node rejected the broadcast: %s: %s", ret.Code, ret.Message)
	}

	rawData, err := proto.Marshal(tx.RawData)
	if err != nil {
		return "", fmt.Errorf("dispatch: computing txid after a successful broadcast: %w", err)
	}
	digest := txbuild.Digest(rawData)
	return hex.EncodeToString(digest[:]), nil
}

// HTTPFinalityReader implements FinalityReader against a real TRON
// node's solidity (SR-confirmed) HTTP surface -- verified live against
// api.trongrid.io while building this chunk: GET
// /walletsolidity/gettransactioninfobyid?value=<txid> returns a non-empty
// body with a matching "id" field once and only once a transaction has
// reached solidity (finalized) state; querying the same id against the
// REGULAR (non-solidity) endpoint would reflect it far sooner, before SR
// consensus actually finalizes it, which is exactly the distinction this
// chunk needs and the "solidity" node exists to provide.
type HTTPFinalityReader struct {
	baseURL string
	http    *http.Client
}

// NewHTTPFinalityReader returns an HTTPFinalityReader for baseURL (e.g.
// "https://api.trongrid.io").
func NewHTTPFinalityReader(baseURL string) *HTTPFinalityReader {
	return &HTTPFinalityReader{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

type transactionInfoResponse struct {
	ID string `json:"id"`
}

// IsFinal reports whether tronTxID has reached SR finality.
func (r *HTTPFinalityReader) IsFinal(ctx context.Context, tronTxID string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		r.baseURL+"/walletsolidity/gettransactioninfobyid?value="+tronTxID, nil)
	if err != nil {
		return false, fmt.Errorf("dispatch: building finality request: %w", err)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("dispatch: checking finality for %s: %w", tronTxID, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("dispatch: reading finality response for %s: %w", tronTxID, err)
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("dispatch: TRON node returned %d checking finality for %s", resp.StatusCode, tronTxID)
	}

	var info transactionInfoResponse
	if err := json.Unmarshal(body, &info); err != nil {
		return false, fmt.Errorf("dispatch: decoding finality response for %s: %w", tronTxID, err)
	}
	return info.ID == tronTxID, nil
}
