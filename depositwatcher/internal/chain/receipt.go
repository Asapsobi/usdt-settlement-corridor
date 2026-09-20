package chain

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

// TransactionBlockNumber tries each configured provider in order and
// returns the block number the first one to answer successfully
// reports -- NOT the 2-provider-agreement bar LogsAt/LatestFinalized
// enforce, and deliberately so: this only locates WHERE to look. The
// actual deposit-crediting evidence -- the Transfer log itself -- is
// then read via LogsAt for that single block, which still enforces the
// real invariant-5 agreement bar before anything downstream can credit
// a customer. A wrong or malicious answer here just means LogsAt finds
// no matching log (or the wrong one, still cross-checked and still
// requiring the log's own tx_hash to match what the caller asked for)
// -- it can never single-handedly authorize a credit.
//
// Tries every provider, not just providers[0], because a real, live
// free-tier provider was found to categorically refuse
// eth_getTransactionReceipt (not an archive-depth thing -- confirmed
// against a transaction from the current block, not just an old one)
// while perfectly capable of every other call this package makes
// against it. Advisory reads elsewhere (ERC20BalanceOf, HeaderByNumber)
// stay primary-only; this one specifically needs the fallback because
// eth_getTransactionReceipt support turned out to vary by provider in a
// way none of this package's other calls have hit.
func (p *Pool) TransactionBlockNumber(ctx context.Context, txHash common.Hash) (uint64, error) {
	var lastErr error
	for _, prov := range p.providers {
		receipt, err := prov.Client.TransactionReceipt(ctx, txHash)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", prov.Name, err)
			continue
		}
		if receipt.BlockNumber == nil {
			lastErr = fmt.Errorf("%s: receipt for %s has no block number (not yet mined?)", prov.Name, txHash.Hex())
			continue
		}
		return receipt.BlockNumber.Uint64(), nil
	}
	return 0, fmt.Errorf("chain: fetching receipt for %s: every provider failed, last error: %w", txHash.Hex(), lastErr)
}
