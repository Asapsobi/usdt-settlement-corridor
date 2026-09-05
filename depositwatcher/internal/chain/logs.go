package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"sort"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// ErrLogMismatch means the primary and cross-check providers returned
// different logs for the identical query -- a hard error, per this
// chunk's spec: "never silently resolved by trust the primary."
var ErrLogMismatch = errors.New("chain: providers returned different logs for the same query")

// LogsAt fetches logs in [fromBlock, toBlock] for contractAddress
// matching topics from the primary provider (providers[0], by
// configuration order -- provider ordering is meaningful for this call),
// then cross-checks the FULL result set against a second provider before
// returning. At this system's real volume (~4 deposits/hour peak, per
// component-map.md) checking everything rather than sampling costs
// nothing, so that's what this does rather than the sampling this
// chunk's spec allows as an alternative.
func (p *Pool) LogsAt(ctx context.Context, fromBlock, toBlock uint64, contractAddress common.Address, topics [][]common.Hash) ([]types.Log, error) {
	if len(p.providers) < 2 {
		// NewPool already refuses fewer than 2 providers, so this is
		// unreachable in practice -- guarded anyway since the cross-check
		// below assumes providers[1] exists.
		return nil, ErrTooFewProviders
	}

	query := ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(fromBlock),
		ToBlock:   new(big.Int).SetUint64(toBlock),
		Addresses: []common.Address{contractAddress},
		Topics:    topics,
	}

	primary, secondary := p.providers[0], p.providers[1]

	primaryLogs, err := primary.Client.FilterLogs(ctx, query)
	if err != nil {
		p.recordFailure(primary.Name, err)
		return nil, fmt.Errorf("chain: primary provider %s: %w", primary.Name, err)
	}

	secondaryLogs, err := secondary.Client.FilterLogs(ctx, query)
	if err != nil {
		p.recordFailure(secondary.Name, err)
		return nil, fmt.Errorf("chain: cross-check provider %s: %w", secondary.Name, err)
	}

	if !logsEqual(primaryLogs, secondaryLogs) {
		p.recordFailure(secondary.Name, fmt.Errorf("returned logs that disagree with primary %s for blocks [%d,%d]",
			primary.Name, fromBlock, toBlock))
		return nil, fmt.Errorf("%w: %s and %s disagree for blocks [%d,%d] (%d vs %d logs)",
			ErrLogMismatch, primary.Name, secondary.Name, fromBlock, toBlock, len(primaryLogs), len(secondaryLogs))
	}

	p.recordSuccess(primary.Name)
	p.recordSuccess(secondary.Name)
	return primaryLogs, nil
}

// logsEqual compares two log sets on their consensus-meaningful fields,
// after sorting both into the same canonical (block, tx index, log
// index) order -- a well-formed provider's response should already be in
// that order, but normalizing before comparing avoids a false mismatch
// from two otherwise-correct providers that merely differ in literal
// response ordering.
func logsEqual(a, b []types.Log) bool {
	if len(a) != len(b) {
		return false
	}
	sortLogs(a)
	sortLogs(b)
	for i := range a {
		if !logEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func sortLogs(logs []types.Log) {
	sort.Slice(logs, func(i, j int) bool {
		if logs[i].BlockNumber != logs[j].BlockNumber {
			return logs[i].BlockNumber < logs[j].BlockNumber
		}
		if logs[i].TxIndex != logs[j].TxIndex {
			return logs[i].TxIndex < logs[j].TxIndex
		}
		return logs[i].Index < logs[j].Index
	})
}

// logEqual deliberately excludes BlockHash and Removed: two providers
// observing the exact same log around a benign, momentary reorg could
// disagree on those fields even when the log's own content -- what
// actually happened -- is identical and correct. Comparing the fields
// that define the event itself, not each provider's view of exactly
// when/how it was included, is the right level of strictness for this
// cross-check.
func logEqual(a, b types.Log) bool {
	return a.Address == b.Address &&
		reflect.DeepEqual(a.Topics, b.Topics) &&
		reflect.DeepEqual(a.Data, b.Data) &&
		a.BlockNumber == b.BlockNumber &&
		a.TxHash == b.TxHash &&
		a.TxIndex == b.TxIndex &&
		a.Index == b.Index
}
