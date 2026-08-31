package journal

import (
	"context"
	"errors"
	"fmt"

	"ledger/internal/accounts"
	"ledger/internal/money"
)

var (
	ErrTooFewLines           = errors.New("journal: entry must have at least two lines")
	ErrTooManyLines          = errors.New("journal: entry has more lines than a journal entry may ever hold")
	ErrZeroAmountLine        = errors.New("journal: line amount must be nonzero")
	ErrAssetMismatch         = errors.New("journal: line asset does not match its account's asset")
	ErrUnbalanced            = errors.New("journal: entry does not balance to zero for at least one asset")
	ErrInvalidIdempotencyKey = errors.New("journal: invalid idempotency key")
)

// maxLines guards the int-to-int16 cast when assigning Seq. It is nowhere
// near a real constraint -- the largest MVP batch (Sweep tier) is on the
// order of 50 recipients -- but a silent seq wraparound in a financial
// ledger is exactly the kind of bug this project does not allow, so it is
// rejected explicitly rather than left to overflow quietly.
const maxLines = 32767

// maxIdempotencyKeyLen matches migrations/0004_idempotency_key_length.sql's
// CHECK constraint -- enforced here too so a bad key is rejected before
// ever reaching the database, not just when it gets there.
const maxIdempotencyKeyLen = 255

func validateIdempotencyKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: empty", ErrInvalidIdempotencyKey)
	}
	if len(key) > maxIdempotencyKeyLen {
		return fmt.Errorf("%w: %d bytes, max %d", ErrInvalidIdempotencyKey, len(key), maxIdempotencyKeyLen)
	}
	return nil
}

// validate checks an EntryRequest against every Go-layer rule from C1.2
// and C1.3, and resolves each line's account. It is the first of the
// enforcement layers described in migrations/0003_journal.sql and
// migrations/0004_idempotency_key_length.sql; the others run in Postgres
// regardless of what this function does.
func validate(ctx context.Context, q accounts.Queryer, req EntryRequest) ([]resolvedLine, error) {
	if err := validateIdempotencyKey(req.IdempotencyKey); err != nil {
		return nil, err
	}
	if len(req.Lines) < 2 {
		return nil, fmt.Errorf("%w: got %d", ErrTooFewLines, len(req.Lines))
	}
	if len(req.Lines) > maxLines {
		return nil, fmt.Errorf("%w: got %d, max %d", ErrTooManyLines, len(req.Lines), maxLines)
	}

	resolved := make([]resolvedLine, len(req.Lines))
	sums := make(map[money.Asset]money.Amount)

	for i, line := range req.Lines {
		if line.Amount.Units == 0 {
			return nil, fmt.Errorf("%w: line %d (%s)", ErrZeroAmountLine, i, line.AccountCode)
		}

		acc, err := accounts.GetByCode(ctx, q, line.AccountCode)
		if err != nil {
			return nil, fmt.Errorf("journal: line %d: %w", i, err)
		}
		if line.Amount.Asset != acc.Asset {
			return nil, fmt.Errorf("%w: line %d: account %s holds %s, line carries %s",
				ErrAssetMismatch, i, acc.Code, acc.Asset, line.Amount.Asset)
		}

		sum, ok := sums[acc.Asset]
		if !ok {
			sum = money.Amount{Asset: acc.Asset, Units: 0}
		}
		sum, err = sum.Add(line.Amount)
		if err != nil {
			return nil, fmt.Errorf("journal: summing asset %s: %w", acc.Asset, err)
		}
		sums[acc.Asset] = sum

		resolved[i] = resolvedLine{seq: int16(i), account: acc, amount: line.Amount}
	}

	for asset, sum := range sums {
		if sum.Units != 0 {
			return nil, fmt.Errorf("%w: %s sums to %d", ErrUnbalanced, asset, sum.Units)
		}
	}

	return resolved, nil
}
