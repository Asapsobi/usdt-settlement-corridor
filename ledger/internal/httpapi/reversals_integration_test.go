//go:build integration

// C1.11 acceptance: the reversal surface over HTTP. Reuses the helpers in
// httpapi_integration_test.go (same package) -- testServer,
// testServerIsolated, doRequest, decodeError, createOrderViaHTTP.
//
// Every test here drives the API exactly as C2 will: no test calls
// journal.Reverse or orders.HandleDepositReorg directly, because the whole
// point of this chunk is that a remote caller no longer has to.
package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"ledger/internal/accounts"
	"ledger/internal/money"
)

type entryLine struct {
	Seq         int16  `json:"seq"`
	AccountCode string `json:"account_code"`
	Asset       string `json:"asset"`
	Amount      string `json:"amount"`
}

type entryDTO struct {
	ID             int64       `json:"id"`
	IdempotencyKey string      `json:"idempotency_key"`
	EntryType      string      `json:"entry_type"`
	Actor          string      `json:"actor"`
	ReversalOf     *int64      `json:"reversal_of"`
	Lines          []entryLine `json:"lines"`
	Outcome        string      `json:"outcome"`
}

type alreadyReversedDTO struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	ExistingReversal *entryDTO `json:"existing_reversal"`
}

// fundedOrder walks a fresh order quoted -> funded over HTTP and returns
// it alongside the idempotency key of the deposit entry that funded it --
// which is all C2 ever has to work from.
func fundedOrder(t *testing.T, baseURL string, pool *pgxpool.Pool) (order map[string]any, depositKey string) {
	t.Helper()
	ctx := context.Background()
	order = createOrderViaHTTP(t, baseURL)
	suffix := uniqueSuffix(t)

	depositAcc := "asset:bsc:deposit:" + suffix
	custBEP := "liability:customer:" + suffix + ":USDT_BEP20"
	_, err := accounts.Create(ctx, pool, depositAcc, accounts.Asset, money.USDT_BEP20)
	require.NoError(t, err)
	_, err = accounts.Create(ctx, pool, custBEP, accounts.Liability, money.USDT_BEP20)
	require.NoError(t, err)

	depositKey = "httpapi_test:deposit:" + suffix
	body := map[string]any{
		"to_state":         "funded",
		"expected_version": order["version"],
		"reason":           "deposit reached 15 confirmations",
		"occurred_at":      time.Now().Format(time.RFC3339),
		"entry": map[string]any{
			"entry_type":  "deposit_final",
			"occurred_at": time.Now().Format(time.RFC3339),
			"lines": []map[string]any{
				{"account_code": depositAcc, "asset": "USDT_BEP20", "amount": "3000.000000"},
				{"account_code": custBEP, "asset": "USDT_BEP20", "amount": "-3000.000000"},
			},
		},
	}
	resp := doRequest(t, http.MethodPost, orderURL(baseURL, order, "/transitions"), depositKey, body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	decodeInto(t, resp, &order)
	require.Equal(t, "funded", order["state"])
	return order, depositKey
}

func orderURL(baseURL string, order map[string]any, suffix string) string {
	return baseURL + "/v1/orders/" + order["external_id"].(string) + suffix
}

// entryByKey exercises GET /v1/entries?idempotency_key=... -- the lookup
// that turns the key a producer knows into the id Reverse needs.
func entryByKey(t *testing.T, baseURL, key string) entryDTO {
	t.Helper()
	resp := doRequest(t, http.MethodGet, baseURL+"/v1/entries?idempotency_key="+key, "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var e entryDTO
	decodeInto(t, resp, &e)
	return e
}

func reverseEntry(t *testing.T, baseURL string, entryID int64, reason string) *http.Response {
	t.Helper()
	return doRequest(t, http.MethodPost,
		fmt.Sprintf("%s/v1/entries/%d/reversal", baseURL, entryID), idemKey(t),
		map[string]any{"reason": reason, "occurred_at": time.Now().Format(time.RFC3339)})
}

// ---------------------------------------------------------------------
// Entry lookup
// ---------------------------------------------------------------------

func TestGetEntryByIDAndByKey(t *testing.T) {
	baseURL, pool := testServer(t)
	ctx := context.Background()
	acc1, acc2 := twoTRXAccounts(t, ctx, pool)

	key := idemKey(t)
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/entries", key, simpleEntryBody(t, acc1, acc2))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var posted entryDTO
	decodeInto(t, resp, &posted)

	byKey := entryByKey(t, baseURL, key)
	require.Equal(t, posted.ID, byKey.ID)
	require.Len(t, byKey.Lines, 2)

	resp = doRequest(t, http.MethodGet, fmt.Sprintf("%s/v1/entries/%d", baseURL, posted.ID), "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var byID entryDTO
	decodeInto(t, resp, &byID)
	require.Equal(t, key, byID.IdempotencyKey)
	require.Len(t, byID.Lines, 2)
}

func TestGetEntryNotFound(t *testing.T) {
	baseURL, _ := testServer(t)

	resp := doRequest(t, http.MethodGet, baseURL+"/v1/entries/999999999", "", nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Equal(t, "entry_not_found", decodeError(t, resp).Error.Code)

	resp = doRequest(t, http.MethodGet, baseURL+"/v1/entries?idempotency_key=nope-"+uniqueSuffix(t), "", nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Equal(t, "entry_not_found", decodeError(t, resp).Error.Code)
}

func TestGetEntryByKeyRequiresTheParameter(t *testing.T) {
	baseURL, _ := testServer(t)
	resp := doRequest(t, http.MethodGet, baseURL+"/v1/entries", "", nil)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "invalid_request", decodeError(t, resp).Error.Code)
}

// ---------------------------------------------------------------------
// POST /v1/entries/{id}/reversal
// ---------------------------------------------------------------------

func TestReverseEntryNegatesEveryLine(t *testing.T) {
	baseURL, pool := testServer(t)
	ctx := context.Background()
	acc1, acc2 := twoTRXAccounts(t, ctx, pool)

	key := idemKey(t)
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/entries", key, simpleEntryBody(t, acc1, acc2))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var original entryDTO
	decodeInto(t, resp, &original)

	resp = reverseEntry(t, baseURL, original.ID, "test reversal")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var reversal entryDTO
	decodeInto(t, resp, &reversal)

	require.Equal(t, "reversal", reversal.EntryType)
	require.NotNil(t, reversal.ReversalOf)
	require.Equal(t, original.ID, *reversal.ReversalOf)
	// The key is derived from the original's, never from the request's.
	require.Equal(t, "ledger:reverse:"+key, reversal.IdempotencyKey)
	// The actor is the authenticated caller, as on every other write.
	require.Equal(t, testActor, reversal.Actor)

	// Same accounts, same seq order, opposite sign on every line.
	require.Len(t, reversal.Lines, len(original.Lines))
	for i, line := range reversal.Lines {
		require.Equal(t, original.Lines[i].AccountCode, line.AccountCode)
		require.Equal(t, original.Lines[i].Asset, line.Asset)
		require.Equal(t, negateDecimal(original.Lines[i].Amount), line.Amount)
	}

	// Both accounts are back to where they started.
	for _, code := range []string{acc1, acc2} {
		resp := doRequest(t, http.MethodGet, baseURL+"/v1/accounts/"+code+"/balance", "", nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var b struct {
			Balance string `json:"balance"`
		}
		decodeInto(t, resp, &b)
		require.Equal(t, "0.000000", b.Balance, "account %s should net to zero after reversal", code)
	}
}

// negateDecimal flips the sign of a decimal string as money.Format would
// render it, so a reversal's lines can be compared to the original's
// directly rather than through a numeric round-trip.
func negateDecimal(s string) string {
	if after, ok := strings.CutPrefix(s, "-"); ok {
		return after
	}
	return "-" + s
}

func TestReverseEntryTwiceConflictsAndReturnsTheExistingReversal(t *testing.T) {
	baseURL, pool := testServer(t)
	ctx := context.Background()
	acc1, acc2 := twoTRXAccounts(t, ctx, pool)

	resp := doRequest(t, http.MethodPost, baseURL+"/v1/entries", idemKey(t), simpleEntryBody(t, acc1, acc2))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var original entryDTO
	decodeInto(t, resp, &original)

	resp = reverseEntry(t, baseURL, original.ID, "first")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var first entryDTO
	decodeInto(t, resp, &first)

	// Second attempt: loud conflict, nothing written -- but it carries the
	// reversal that exists, so a caller whose first call timed out can tell
	// that its own request is what created it.
	resp = reverseEntry(t, baseURL, original.ID, "second")
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	var conflict alreadyReversedDTO
	decodeInto(t, resp, &conflict)
	require.Equal(t, "already_reversed", conflict.Error.Code)
	require.NotNil(t, conflict.ExistingReversal, "409 must carry the reversal that exists")
	require.Equal(t, first.ID, conflict.ExistingReversal.ID)
}

func TestReverseAReversalIsRejected(t *testing.T) {
	baseURL, pool := testServer(t)
	ctx := context.Background()
	acc1, acc2 := twoTRXAccounts(t, ctx, pool)

	resp := doRequest(t, http.MethodPost, baseURL+"/v1/entries", idemKey(t), simpleEntryBody(t, acc1, acc2))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var original entryDTO
	decodeInto(t, resp, &original)

	resp = reverseEntry(t, baseURL, original.ID, "first")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var reversal entryDTO
	decodeInto(t, resp, &reversal)

	resp = reverseEntry(t, baseURL, reversal.ID, "reversing a reversal")
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	require.Equal(t, "cannot_reverse_a_reversal", decodeError(t, resp).Error.Code)
}

func TestReverseEntryNotFound(t *testing.T) {
	baseURL, _ := testServer(t)
	resp := reverseEntry(t, baseURL, 999999999, "no such entry")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Equal(t, "entry_not_found", decodeError(t, resp).Error.Code)
}

func TestReverseEntryRequiresReasonAndOccurredAt(t *testing.T) {
	baseURL, pool := testServer(t)
	ctx := context.Background()
	acc1, acc2 := twoTRXAccounts(t, ctx, pool)

	resp := doRequest(t, http.MethodPost, baseURL+"/v1/entries", idemKey(t), simpleEntryBody(t, acc1, acc2))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var original entryDTO
	decodeInto(t, resp, &original)

	resp = doRequest(t, http.MethodPost,
		fmt.Sprintf("%s/v1/entries/%d/reversal", baseURL, original.ID), idemKey(t),
		map[string]any{"occurred_at": time.Now().Format(time.RFC3339)}) // no reason
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "invalid_request", decodeError(t, resp).Error.Code)
}

func TestReverseEntryRequiresIdempotencyKeyHeader(t *testing.T) {
	baseURL, _ := testServer(t)
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/entries/1/reversal", "",
		map[string]any{"reason": "x", "occurred_at": time.Now().Format(time.RFC3339)})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "invalid_request", decodeError(t, resp).Error.Code)
}

// ---------------------------------------------------------------------
// entry_id on a transition -- the other half of the gap
// ---------------------------------------------------------------------

// TestTransitionWithEntryIDLinksARealReversal is the case that could not
// be expressed over HTTP before C1.11: funded -> quoted, caused by a
// genuine reversal rather than a hand-built negating entry.
func TestTransitionWithEntryIDLinksARealReversal(t *testing.T) {
	baseURL, pool := testServer(t)
	order, depositKey := fundedOrder(t, baseURL, pool)

	deposit := entryByKey(t, baseURL, depositKey)
	resp := reverseEntry(t, baseURL, deposit.ID, "deposit reorged out")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var reversal entryDTO
	decodeInto(t, resp, &reversal)

	body := map[string]any{
		"to_state":         "quoted",
		"expected_version": order["version"],
		"reason":           "deposit reorged out before dispatch",
		"occurred_at":      time.Now().Format(time.RFC3339),
		"entry_id":         reversal.ID,
	}
	resp = doRequest(t, http.MethodPost, orderURL(baseURL, order, "/transitions"), idemKey(t), body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var updated map[string]any
	decodeInto(t, resp, &updated)
	require.Equal(t, "quoted", updated["state"])

	// The transition points at the reversal, and nothing new was posted.
	var entryID int64
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT entry_id FROM order_transitions
		 WHERE order_id = $1 AND to_state = 'quoted' ORDER BY id DESC LIMIT 1`,
		int64(order["id"].(float64))).Scan(&entryID))
	require.Equal(t, reversal.ID, entryID)
}

func TestTransitionRejectsBothEntryAndEntryID(t *testing.T) {
	baseURL, pool := testServer(t)
	order, depositKey := fundedOrder(t, baseURL, pool)
	deposit := entryByKey(t, baseURL, depositKey)

	body := map[string]any{
		"to_state":         "quoted",
		"expected_version": order["version"],
		"reason":           "both supplied",
		"occurred_at":      time.Now().Format(time.RFC3339),
		"entry_id":         deposit.ID,
		"entry": map[string]any{
			"entry_type":  "deposit_final",
			"occurred_at": time.Now().Format(time.RFC3339),
			"lines":       []map[string]any{},
		},
	}
	resp := doRequest(t, http.MethodPost, orderURL(baseURL, order, "/transitions"), idemKey(t), body)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "invalid_request", decodeError(t, resp).Error.Code)
}

// ---------------------------------------------------------------------
// POST /v1/orders/{external_id}/reorg
// ---------------------------------------------------------------------

// TestReorgScenarioA: the routine case. Net effect on the books is zero
// and nothing halts.
func TestReorgScenarioA(t *testing.T) {
	baseURL, pool := testServerIsolated(t)
	order, depositKey := fundedOrder(t, baseURL, pool)

	resp := doRequest(t, http.MethodPost, orderURL(baseURL, order, "/reorg"), idemKey(t),
		map[string]any{"original_entry_key": depositKey})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var updated map[string]any
	decodeInto(t, resp, &updated)
	require.Equal(t, "quoted", updated["state"], "scenario A returns the order to quoted")

	// No loss booked, and the ledger is NOT halted.
	resp = doRequest(t, http.MethodGet, baseURL+"/v1/accounts/expense:loss:reorg/balance", "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var loss struct {
		Balance string `json:"balance"`
	}
	decodeInto(t, resp, &loss)
	require.Equal(t, "0.000000", loss.Balance)

	requireHalted(t, baseURL, false, "")
	requireTrialBalanceZero(t, baseURL)
}

// TestReorgScenarioB: the loss case. The payout already left, so the
// deposit is reversed, the full amount_out is booked as a realized loss,
// the ledger HALTS, and the order stays settled -- it really did settle.
func TestReorgScenarioB(t *testing.T) {
	baseURL, pool := testServerIsolated(t)
	order, depositKey := fundedOrder(t, baseURL, pool)
	order = walkToSettled(t, baseURL, pool, order)
	require.Equal(t, "settled", order["state"])

	resp := doRequest(t, http.MethodPost, orderURL(baseURL, order, "/reorg"), idemKey(t),
		map[string]any{"original_entry_key": depositKey})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var after map[string]any
	decodeInto(t, resp, &after)
	require.Equal(t, "settled", after["state"], "the order really did settle; only the funding vanished")

	// The full amount_out is booked as a realized loss.
	resp = doRequest(t, http.MethodGet, baseURL+"/v1/accounts/expense:loss:reorg/balance", "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var loss struct {
		Balance string `json:"balance"`
	}
	decodeInto(t, resp, &loss)
	require.Equal(t, order["amount_out"], loss.Balance)

	// This path must be impossible to take without halting.
	requireHalted(t, baseURL, true, "POST_SETTLEMENT_REORG")
	requireTrialBalanceZero(t, baseURL)
}

func TestReorgUnexpectedState(t *testing.T) {
	baseURL, _ := testServer(t)
	order := createOrderViaHTTP(t, baseURL) // still quoted -- neither scenario applies

	resp := doRequest(t, http.MethodPost, orderURL(baseURL, order, "/reorg"), idemKey(t),
		map[string]any{"original_entry_key": "irrelevant-" + uniqueSuffix(t)})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Equal(t, "unexpected_state", decodeError(t, resp).Error.Code)
}

func TestReorgUnknownEntryKey(t *testing.T) {
	baseURL, pool := testServer(t)
	order, _ := fundedOrder(t, baseURL, pool)

	resp := doRequest(t, http.MethodPost, orderURL(baseURL, order, "/reorg"), idemKey(t),
		map[string]any{"original_entry_key": "never-posted-" + uniqueSuffix(t)})
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Equal(t, "entry_not_found", decodeError(t, resp).Error.Code)
}

func TestReorgRequiresOriginalEntryKey(t *testing.T) {
	baseURL, pool := testServer(t)
	order, _ := fundedOrder(t, baseURL, pool)

	resp := doRequest(t, http.MethodPost, orderURL(baseURL, order, "/reorg"), idemKey(t),
		map[string]any{})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "invalid_request", decodeError(t, resp).Error.Code)
}

func TestReorgOrderNotFound(t *testing.T) {
	baseURL, _ := testServer(t)
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/orders/no-such-order/reorg", idemKey(t),
		map[string]any{"original_entry_key": "x"})
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Equal(t, "order_not_found", decodeError(t, resp).Error.Code)
}

// ---------------------------------------------------------------------
// shared assertions
// ---------------------------------------------------------------------

func requireHalted(t *testing.T, baseURL string, want bool, wantReason string) {
	t.Helper()
	resp := doRequest(t, http.MethodGet, baseURL+"/v1/system/halt", "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var h struct {
		Halted bool   `json:"halted"`
		Reason string `json:"reason"`
	}
	decodeInto(t, resp, &h)
	require.Equal(t, want, h.Halted)
	if wantReason != "" {
		require.Equal(t, wantReason, h.Reason)
	}
}

func requireTrialBalanceZero(t *testing.T, baseURL string) {
	t.Helper()
	resp := doRequest(t, http.MethodGet, baseURL+"/v1/trial-balance", "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var trial map[string]string
	decodeInto(t, resp, &trial)
	for asset, total := range trial {
		require.Regexp(t, `^0(\.0+)?$`, total, "trial balance for %s must be zero", asset)
	}
}

// walkToSettled drives a funded order through screened -> dispatching ->
// settled over HTTP, posting the §B conversion and payout entries.
func walkToSettled(t *testing.T, baseURL string, pool *pgxpool.Pool, order map[string]any) map[string]any {
	t.Helper()
	ctx := context.Background()
	suffix := uniqueSuffix(t)

	custBEP := "liability:customer:" + suffix + ":USDT_BEP20"
	custTRC := "liability:customer:" + suffix + ":USDT_TRC20"
	slot := "asset:tron:slot:" + suffix
	_, err := accounts.Create(ctx, pool, custBEP, accounts.Liability, money.USDT_BEP20)
	require.NoError(t, err)
	_, err = accounts.Create(ctx, pool, custTRC, accounts.Liability, money.USDT_TRC20)
	require.NoError(t, err)
	_, err = accounts.Create(ctx, pool, slot, accounts.Asset, money.USDT_TRC20)
	require.NoError(t, err)

	transition := func(to, reason string, entry map[string]any) {
		body := map[string]any{
			"to_state":         to,
			"expected_version": order["version"],
			"reason":           reason,
			"occurred_at":      time.Now().Format(time.RFC3339),
		}
		if entry != nil {
			body["entry"] = entry
		}
		resp := doRequest(t, http.MethodPost, orderURL(baseURL, order, "/transitions"), idemKey(t), body)
		require.Equal(t, http.StatusOK, resp.StatusCode, "transition to %s", to)
		decodeInto(t, resp, &order)
	}

	transition("screened", "screening verdict pass", nil)

	amountIn := order["amount_in"].(string)
	amountOut := order["amount_out"].(string)
	transition("dispatching", "posting the conversion entry", map[string]any{
		"entry_type":  "conversion",
		"occurred_at": time.Now().Format(time.RFC3339),
		"lines": []map[string]any{
			{"account_code": custBEP, "asset": "USDT_BEP20", "amount": amountIn},
			{"account_code": "position:corridor:USDT_BEP20", "asset": "USDT_BEP20", "amount": "-" + amountIn},
			{"account_code": "position:corridor:USDT_TRC20", "asset": "USDT_TRC20", "amount": amountIn},
			{"account_code": custTRC, "asset": "USDT_TRC20", "amount": "-" + amountOut},
			{"account_code": "revenue:fee", "asset": "USDT_TRC20", "amount": "-" + order["fee_units"].(string)},
			{"account_code": "revenue:network_fee", "asset": "USDT_TRC20", "amount": "-" + order["network_fee_units"].(string)},
		},
	})

	transition("settled", "payout SR-final", map[string]any{
		"entry_type":  "payout_settled",
		"occurred_at": time.Now().Format(time.RFC3339),
		"lines": []map[string]any{
			{"account_code": custTRC, "asset": "USDT_TRC20", "amount": amountOut},
			{"account_code": slot, "asset": "USDT_TRC20", "amount": "-" + amountOut},
		},
	})
	return order
}
