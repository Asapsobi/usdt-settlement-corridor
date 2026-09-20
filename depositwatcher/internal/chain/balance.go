package chain

import (
	"context"
	"fmt"
	"math/big"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// ERC20BalanceOf queries the primary provider only (providers[0]), same
// convention as LatestBlockHeader/HeaderByNumber -- this is the ops
// console's own Sweep page (docs/03-build/ops-console-build-prompts.md's
// OC.11), a read-only convenience an operator uses to decide whether an
// address is worth sweeping. No money-movement decision inside this
// service depends on this value, unlike LatestFinalized/LogsAt, so it
// isn't held to the same 2-provider-agreement bar.
func (p *Pool) ERC20BalanceOf(ctx context.Context, contractAddress, holder common.Address) (*big.Int, error) {
	methodID := crypto.Keccak256([]byte("balanceOf(address)"))[:4]
	data := append(methodID, common.LeftPadBytes(holder.Bytes(), 32)...)
	result, err := p.providers[0].Client.CallContract(ctx, ethereum.CallMsg{To: &contractAddress, Data: data}, nil)
	if err != nil {
		return nil, fmt.Errorf("chain: balanceOf(%s) at %s: %w", holder.Hex(), contractAddress.Hex(), err)
	}
	return new(big.Int).SetBytes(result), nil
}
