package chain

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// LatestBlockHeader and HeaderByNumber deliberately query only the
// primary provider (providers[0], the same convention LogsAt uses) --
// unlike LatestFinalized and LogsAt, which require cross-provider
// agreement before trusting anything. This data feeds ONLY the advisory,
// pre-final reorg observability in RunIngestionLoop (C2.3), which by
// design "never by itself triggers a report to C1" (see the C2 build
// spec's §B). Requiring 2-provider agreement for every block in a fast,
// continuous forward scan would add real cost for data that is
// explicitly non-critical; the money-relevant finality decision goes
// through LatestFinalized, which does require it.

// LatestBlockHeader returns the primary provider's current chain tip.
func (p *Pool) LatestBlockHeader(ctx context.Context) (*types.Header, error) {
	return p.providers[0].Client.HeaderByNumber(ctx, nil)
}

// HeaderByNumber returns the primary provider's header at the given
// height.
func (p *Pool) HeaderByNumber(ctx context.Context, height uint64) (*types.Header, error) {
	return p.providers[0].Client.HeaderByNumber(ctx, new(big.Int).SetUint64(height))
}
