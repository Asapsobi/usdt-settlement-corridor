package replay

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"ledger/internal/accounts"
	"ledger/internal/money"
)

// sharedAccounts is the pool every order draws from. Customers and slots
// are created once, up front, and reused across many orders -- matching
// the real system (a returning customer reuses their liability account;
// there are only six payout slots, ever) and keeping account setup cost
// roughly constant instead of scaling with NumOrders. Deposit accounts
// are the one thing genuinely unique per order (one HD address per
// order, per C2's real design), so those are created on demand inside
// each order's own scenario function, not here.
type sharedAccounts struct {
	customerBEP []string // liability:customer:<n>:USDT_BEP20
	customerTRC []string // liability:customer:<n>:USDT_TRC20, same customer index as customerBEP
	slots       []string // asset:tron:slot:<n>
}

const (
	numCustomers = 200
	numSlots     = 6
)

func setupSharedAccounts(ctx context.Context, pool *pgxpool.Pool, runTag string) (*sharedAccounts, error) {
	s := &sharedAccounts{
		customerBEP: make([]string, numCustomers),
		customerTRC: make([]string, numCustomers),
		slots:       make([]string, numSlots),
	}

	for i := 0; i < numCustomers; i++ {
		bep := fmt.Sprintf("liability:customer:replay-%s-%d:USDT_BEP20", runTag, i)
		trc := fmt.Sprintf("liability:customer:replay-%s-%d:USDT_TRC20", runTag, i)
		if _, err := accounts.Create(ctx, pool, bep, accounts.Liability, money.USDT_BEP20); err != nil {
			return nil, fmt.Errorf("replay: creating customer account %q: %w", bep, err)
		}
		if _, err := accounts.Create(ctx, pool, trc, accounts.Liability, money.USDT_TRC20); err != nil {
			return nil, fmt.Errorf("replay: creating customer account %q: %w", trc, err)
		}
		s.customerBEP[i] = bep
		s.customerTRC[i] = trc
	}

	for i := 0; i < numSlots; i++ {
		slot := fmt.Sprintf("asset:tron:slot:replay-%s-%d", runTag, i)
		if _, err := accounts.Create(ctx, pool, slot, accounts.Asset, money.USDT_TRC20); err != nil {
			return nil, fmt.Errorf("replay: creating slot account %q: %w", slot, err)
		}
		s.slots[i] = slot
	}

	return s, nil
}

// pick returns a customer's (BEP20, TRC20) account pair, deterministically
// selected by rng so runs with the same seed pick the same customers.
func (s *sharedAccounts) pickCustomer(rngIntn func(int) int) (bep, trc string) {
	i := rngIntn(numCustomers)
	return s.customerBEP[i], s.customerTRC[i]
}

func (s *sharedAccounts) pickSlot(rngIntn func(int) int) string {
	return s.slots[rngIntn(numSlots)]
}
