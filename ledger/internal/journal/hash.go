package journal

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// hashLine and hashPayload exist only to give canonicalHash a stable JSON
// shape; they are never stored or returned.
type hashLine struct {
	AccountCode string `json:"account_code"`
	Asset       string `json:"asset"`
	Amount      int64  `json:"amount"`
}

type hashPayload struct {
	EntryType  string     `json:"entry_type"`
	OrderID    *int64     `json:"order_id"`
	Lines      []hashLine `json:"lines"`
	OccurredAt string     `json:"occurred_at"`
}

// canonicalHash computes the sha256 of a deterministic JSON encoding of
// req: entry_type, order_id, lines sorted by (account_code, asset,
// amount), and occurred_at truncated to microseconds. Sorting the lines
// means line order in the request never changes the hash; truncating
// occurred_at means two calls differing by less than a microsecond hash
// identically, while calls differing by a microsecond or more do not --
// both are required by C1.3's idempotency semantics, computed here because
// this is where the hash itself lives.
func canonicalHash(req EntryRequest) ([]byte, error) {
	lines := make([]hashLine, len(req.Lines))
	for i, l := range req.Lines {
		lines[i] = hashLine{
			AccountCode: l.AccountCode,
			Asset:       string(l.Amount.Asset),
			Amount:      l.Amount.Units,
		}
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].AccountCode != lines[j].AccountCode {
			return lines[i].AccountCode < lines[j].AccountCode
		}
		if lines[i].Asset != lines[j].Asset {
			return lines[i].Asset < lines[j].Asset
		}
		return lines[i].Amount < lines[j].Amount
	})

	payload := hashPayload{
		EntryType:  req.EntryType,
		OrderID:    req.OrderID,
		Lines:      lines,
		OccurredAt: req.OccurredAt.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano),
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("journal: hashing payload: %w", err)
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}
