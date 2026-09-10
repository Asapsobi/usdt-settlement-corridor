package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gateway/internal/c2client"
	"gateway/internal/db"
	"gateway/internal/httpapi"
)

// --- small HTTP helpers driving h.router in-process ---------------------

type quoteResp struct {
	QuoteID int64 `json:"quote_id"`
}

func (h *harness) doJSON(method, path, rawKey, idempotencyKey string, body any) (int, []byte) {
	var reader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if rawKey != "" {
		req.Header.Set("Authorization", "Bearer "+rawKey)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func (h *harness) issueQuote(rawKey string) (quoteResp, error) {
	status, body := h.doJSON(http.MethodPost, "/v1/quotes", rawKey, "",
		map[string]any{"tier": "STANDARD", "amount_in": "3000.000000", "recipient_address": "TReplayRecipient"})
	if status != http.StatusCreated {
		return quoteResp{}, fmt.Errorf("POST /v1/quotes: status %d: %s", status, body)
	}
	var q quoteResp
	if err := json.Unmarshal(body, &q); err != nil {
		return quoteResp{}, fmt.Errorf("decoding quote response: %w: %s", err, body)
	}
	return q, nil
}

func (h *harness) createOrder(rawKey string, quoteID int64, externalID, idempotencyKey string) (int, map[string]any) {
	status, body := h.doJSON(http.MethodPost, "/v1/orders", rawKey, idempotencyKey,
		map[string]any{"quote_id": quoteID, "external_id": externalID})
	var resp map[string]any
	json.Unmarshal(body, &resp)
	return status, resp
}

func (h *harness) getOrderStatus(rawKey, externalID string) (int, map[string]any) {
	status, body := h.doJSON(http.MethodGet, "/v1/orders/"+externalID, rawKey, "", nil)
	var resp map[string]any
	json.Unmarshal(body, &resp)
	return status, resp
}

// --- scenarios ------------------------------------------------------------

// scenarioCleanSettleWithWebhook is the SCENARIO MIX's first row: quote
// -> order -> settled -> webhook delivered.
func (h *harness) scenarioCleanSettleWithWebhook() Result {
	const name = "CleanQuoteOrderSettledWebhookDelivered"
	return runGuarded(name, func() error {
		var received atomic.Bool
		var receivedBody []byte
		var mu sync.Mutex
		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			receivedBody, _ = jsonBody(r)
			mu.Unlock()
			received.Store(true)
			w.WriteHeader(http.StatusOK)
		}))
		defer webhookSrv.Close()

		cust, rawKey, err := h.customersStore.Create(h.ctx, "replay-clean-co")
		if err != nil {
			return fmt.Errorf("creating customer: %w", err)
		}
		if err := h.customersStore.SetWebhookURL(h.ctx, cust.ID, webhookSrv.URL); err != nil {
			return fmt.Errorf("setting webhook_url: %w", err)
		}

		quote, err := h.issueQuote(rawKey)
		if err != nil {
			return err
		}
		externalID := h.uniqueExternalID("clean")
		h.prodExternalIDs = append(h.prodExternalIDs, externalID)

		status, orderResp := h.createOrder(rawKey, quote.QuoteID, externalID, "replay:clean:"+externalID)
		if status != http.StatusCreated {
			return fmt.Errorf("POST /v1/orders: status %d: %v", status, orderResp)
		}
		if orderResp["status"] != "created" {
			return fmt.Errorf("order status = %v, want created (C2 address assignment should have succeeded against a real watcherd)", orderResp["status"])
		}

		fo, err := h.ledger.getOrder(h.ctx, externalID)
		if err != nil {
			return err
		}
		if fo, err = h.ledger.fundOrder(h.ctx, h.ledgerPool, fo); err != nil {
			return err
		}
		if fo, err = h.ledger.screenOrder(h.ctx, fo); err != nil {
			return err
		}
		if fo, err = h.ledger.dispatchOrder(h.ctx, h.ledgerPool, fo); err != nil {
			return err
		}
		if _, err = h.ledger.settleOrder(h.ctx, h.ledgerPool, fo, h.nextSlotID()); err != nil {
			return err
		}

		if err := h.trigger.Tick(h.ctx); err != nil {
			return fmt.Errorf("webhook trigger tick: %w", err)
		}
		if err := h.deliverer.Tick(h.ctx); err != nil {
			return fmt.Errorf("webhook deliver tick: %w", err)
		}

		if !received.Load() {
			return fmt.Errorf("webhook was never delivered to this customer's own endpoint")
		}
		mu.Lock()
		var payload map[string]any
		json.Unmarshal(receivedBody, &payload)
		mu.Unlock()
		if payload["external_id"] != externalID || payload["state"] != "settled" {
			return fmt.Errorf("webhook payload = %v, want external_id=%s state=settled", payload, externalID)
		}

		status, statusResp := h.getOrderStatus(rawKey, externalID)
		if status != http.StatusOK || statusResp["status"] != "settled" {
			return fmt.Errorf("GET /v1/orders/%s: status=%d body=%v, want 200 status=settled", externalID, status, statusResp)
		}
		return nil
	})
}

// scenarioQuoteExpiresBeforeOrder is the SCENARIO MIX's second row: an
// expired quote gets 409, and neither C1 nor C2 ever sees a call for
// it.
func (h *harness) scenarioQuoteExpiresBeforeOrder() Result {
	const name = "QuoteExpiresBeforeOrder_NoUpstreamCall"
	return runGuarded(name, func() error {
		cust, rawKey, err := h.customersStore.Create(h.ctx, "replay-expired-co")
		if err != nil {
			return err
		}
		_ = cust

		// A dedicated server/router with a near-zero quote validity, so
		// the quote this scenario issues is already expired by the time
		// it tries to order against it -- same technique C6.3's own
		// integration test uses, against this harness's real C1/C2.
		shortServer := *h.server
		shortServer.QuoteValidity = time.Millisecond
		shortRouter := httpapi.NewRouter(&shortServer)

		req := httptest.NewRequest(http.MethodPost, "/v1/quotes", bytes.NewReader(mustJSON(map[string]any{
			"tier": "STANDARD", "amount_in": "3000.000000", "recipient_address": "TReplayRecipient",
		})))
		req.Header.Set("Authorization", "Bearer "+rawKey)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		shortRouter.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			return fmt.Errorf("issuing quote: status %d: %s", rec.Code, rec.Body.String())
		}
		var q quoteResp
		json.Unmarshal(rec.Body.Bytes(), &q)

		time.Sleep(10 * time.Millisecond)

		externalID := h.uniqueExternalID("expired")
		orderReq := httptest.NewRequest(http.MethodPost, "/v1/orders", bytes.NewReader(mustJSON(map[string]any{
			"quote_id": q.QuoteID, "external_id": externalID,
		})))
		orderReq.Header.Set("Authorization", "Bearer "+rawKey)
		orderReq.Header.Set("Idempotency-Key", "replay:expired:"+externalID)
		orderReq.Header.Set("Content-Type", "application/json")
		orderRec := httptest.NewRecorder()
		shortRouter.ServeHTTP(orderRec, orderReq)
		if orderRec.Code != http.StatusConflict {
			return fmt.Errorf("POST /v1/orders with expired quote: status %d, want 409: %s", orderRec.Code, orderRec.Body.String())
		}

		// No C1 order should exist for this external_id at all.
		if _, err := h.ledger.getOrder(h.ctx, externalID); err == nil {
			return fmt.Errorf("C1 has an order for %s despite the quote having expired before order creation", externalID)
		}
		return nil
	})
}

// scenarioAddressPendingResolvedByReconcile is the SCENARIO MIX's third
// row: C2's own POST /v1/addresses fails after C1's order succeeds ->
// address_pending -> C6.4 resolves it within the next loop tick.
func (h *harness) scenarioAddressPendingResolvedByReconcile() Result {
	const name = "AddressPendingResolvedByReconcile"
	return runGuarded(name, func() error {
		cust, rawKey, err := h.customersStore.Create(h.ctx, "replay-pending-co")
		if err != nil {
			return err
		}
		_ = cust

		// A dedicated server whose Watcher points at an already-closed
		// server -- simulates C2 being unreachable for exactly this
		// order's own step 5, without touching the real watcherd this
		// whole run otherwise depends on.
		deadWatcherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		deadWatcherSrv.Close()

		brokenServer := *h.server
		brokenServer.Watcher = c2client.New(deadWatcherSrv.URL, h.cfg.WatcherToken)
		brokenRouter := httpapi.NewRouter(&brokenServer)

		quoteBody, _ := json.Marshal(map[string]any{"tier": "STANDARD", "amount_in": "3000.000000", "recipient_address": "TReplayRecipient"})
		qReq := httptest.NewRequest(http.MethodPost, "/v1/quotes", bytes.NewReader(quoteBody))
		qReq.Header.Set("Authorization", "Bearer "+rawKey)
		qReq.Header.Set("Content-Type", "application/json")
		qRec := httptest.NewRecorder()
		brokenRouter.ServeHTTP(qRec, qReq)
		if qRec.Code != http.StatusCreated {
			return fmt.Errorf("issuing quote: status %d: %s", qRec.Code, qRec.Body.String())
		}
		var q quoteResp
		json.Unmarshal(qRec.Body.Bytes(), &q)

		externalID := h.uniqueExternalID("pending")
		h.prodExternalIDs = append(h.prodExternalIDs, externalID)
		orderBody, _ := json.Marshal(map[string]any{"quote_id": q.QuoteID, "external_id": externalID})
		oReq := httptest.NewRequest(http.MethodPost, "/v1/orders", bytes.NewReader(orderBody))
		oReq.Header.Set("Authorization", "Bearer "+rawKey)
		oReq.Header.Set("Idempotency-Key", "replay:pending:"+externalID)
		oReq.Header.Set("Content-Type", "application/json")
		oRec := httptest.NewRecorder()
		brokenRouter.ServeHTTP(oRec, oReq)
		if oRec.Code != http.StatusCreated {
			return fmt.Errorf("POST /v1/orders: status %d: %s", oRec.Code, oRec.Body.String())
		}
		var orderResp map[string]any
		json.Unmarshal(oRec.Body.Bytes(), &orderResp)
		if orderResp["status"] != "address_pending" {
			return fmt.Errorf("order status = %v, want address_pending", orderResp["status"])
		}

		gatewayOrder, err := h.ordersStore.Get(h.ctx, externalID)
		if err != nil {
			return err
		}
		if gatewayOrder.C2AddressAssigned {
			return fmt.Errorf("gateway_orders row already shows c2_address_assigned=true before reconcile ran")
		}

		// h.reconciler is wired against the REAL watcherd -- this is the
		// "closes the gap asynchronously" half of the scenario. A short
		// sleep clears the reconciler's own grace period comfortably
		// (already configured to 1ms, but real wall-clock/DB timestamp
		// resolution makes a hair of margin worth having).
		time.Sleep(20 * time.Millisecond)
		if err := h.reconciler.Tick(h.ctx); err != nil {
			return fmt.Errorf("reconcile tick: %w", err)
		}
		resolved, err := h.ordersStore.Get(h.ctx, externalID)
		if err != nil {
			return err
		}
		if !resolved.C2AddressAssigned || resolved.DepositAddress == nil {
			return fmt.Errorf("gateway_orders row for %s is still address_pending after one reconcile tick", externalID)
		}
		return nil
	})
}

// scenarioWebhookExhaustsThenBackstopAgrees is the SCENARIO MIX's
// fourth row: webhook delivery fails for exactly 8 attempts, then the
// pull-based backstop (C6.5) still agrees with the true state.
func (h *harness) scenarioWebhookExhaustsThenBackstopAgrees() Result {
	const name = "WebhookExhaustsThenBackstopAgrees"
	return runGuarded(name, func() error {
		failingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer failingSrv.Close()

		cust, rawKey, err := h.customersStore.Create(h.ctx, "replay-exhaust-co")
		if err != nil {
			return err
		}
		if err := h.customersStore.SetWebhookURL(h.ctx, cust.ID, failingSrv.URL); err != nil {
			return err
		}

		quote, err := h.issueQuote(rawKey)
		if err != nil {
			return err
		}
		externalID := h.uniqueExternalID("exhaust")
		h.prodExternalIDs = append(h.prodExternalIDs, externalID)
		status, orderResp := h.createOrder(rawKey, quote.QuoteID, externalID, "replay:exhaust:"+externalID)
		if status != http.StatusCreated {
			return fmt.Errorf("POST /v1/orders: status %d: %v", status, orderResp)
		}

		fo, err := h.ledger.getOrder(h.ctx, externalID)
		if err != nil {
			return err
		}
		if fo, err = h.ledger.fundOrder(h.ctx, h.ledgerPool, fo); err != nil {
			return err
		}
		if fo, err = h.ledger.screenOrder(h.ctx, fo); err != nil {
			return err
		}
		if fo, err = h.ledger.dispatchOrder(h.ctx, h.ledgerPool, fo); err != nil {
			return err
		}
		if _, err = h.ledger.settleOrder(h.ctx, h.ledgerPool, fo, h.nextSlotID()); err != nil {
			return err
		}

		if err := h.trigger.Tick(h.ctx); err != nil {
			return err
		}
		// Each real failed attempt schedules the next one via exponential
		// backoff (10s+), which a same-process, back-to-back Tick loop
		// would never naturally clear -- force next_attempt_at back to
		// now before every attempt after the first, so this scenario
		// drives all 8 real attempts without an 8-figure real sleep.
		for i := 0; i < webhooksDefaultMaxAttempts; i++ {
			if i > 0 {
				if _, err := h.gatewayPool.Exec(h.ctx,
					`UPDATE webhook_deliveries SET next_attempt_at = now() WHERE external_id = $1 AND event_type = $2`,
					externalID, "settled"); err != nil {
					return fmt.Errorf("forcing next_attempt_at: %w", err)
				}
			}
			if err := h.deliverer.Tick(h.ctx); err != nil {
				return err
			}
		}

		row, err := findWebhookDelivery(h.ctx, h.gatewayPool, externalID, "settled")
		if err != nil {
			return err
		}
		if row.deliveredAt {
			return fmt.Errorf("webhook_deliveries row for %s shows delivered despite the endpoint always failing", externalID)
		}
		if row.attemptCount != webhooksDefaultMaxAttempts {
			return fmt.Errorf("webhook_deliveries.attempt_count = %d, want %d (exhausted)", row.attemptCount, webhooksDefaultMaxAttempts)
		}

		status, statusResp := h.getOrderStatus(rawKey, externalID)
		if status != http.StatusOK || statusResp["status"] != "settled" {
			return fmt.Errorf("GET /v1/orders/%s after webhook exhaustion: status=%d body=%v, want 200 status=settled", externalID, status, statusResp)
		}
		return nil
	})
}

// scenarioConcurrentOrderCreationRacesQuote is the SCENARIO MIX's fifth
// row: two concurrent order-creation requests racing the same quote_id
// -> exactly one order created, the other gets 409
// quote_already_consumed.
func (h *harness) scenarioConcurrentOrderCreationRacesQuote() Result {
	const name = "ConcurrentOrderCreationRacesSameQuote"
	return runGuarded(name, func() error {
		_, rawKey, err := h.customersStore.Create(h.ctx, "replay-race-co")
		if err != nil {
			return err
		}
		quote, err := h.issueQuote(rawKey)
		if err != nil {
			return err
		}

		extA := h.uniqueExternalID("race-a")
		extB := h.uniqueExternalID("race-b")

		var wg sync.WaitGroup
		codes := make([]int, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			codes[0], _ = h.createOrder(rawKey, quote.QuoteID, extA, "replay:race-a:"+extA)
		}()
		go func() {
			defer wg.Done()
			codes[1], _ = h.createOrder(rawKey, quote.QuoteID, extB, "replay:race-b:"+extB)
		}()
		wg.Wait()

		created, conflicted := 0, 0
		for _, c := range codes {
			switch c {
			case http.StatusCreated:
				created++
			case http.StatusConflict:
				conflicted++
			}
		}
		if created != 1 || conflicted != 1 {
			return fmt.Errorf("status codes = %v, want exactly one 201 and one 409", codes)
		}

		h.prodExternalIDs = append(h.prodExternalIDs, extA, extB)
		return nil
	})
}

// scenarioSystemHaltedRefusesQuoteAndOrder is the SCENARIO MIX's sixth
// row: system halted mid-quote and mid-order-creation -> both correctly
// refused, no partial state left behind.
func (h *harness) scenarioSystemHaltedRefusesQuoteAndOrder() Result {
	const name = "SystemHaltedRefusesQuoteAndOrder"
	return runGuarded(name, func() error {
		_, rawKey, err := h.customersStore.Create(h.ctx, "replay-halt-co")
		if err != nil {
			return err
		}

		// A valid, unexpired quote obtained BEFORE halting, so the
		// order-creation attempt below fails on the halt check (step 2),
		// not on an expired/missing quote.
		quote, err := h.issueQuote(rawKey)
		if err != nil {
			return err
		}

		ledgerHalt := &ledgerHaltFixture{baseURL: h.cfg.LedgerBaseURL, token: h.cfg.LedgerToken}
		if err := ledgerHalt.set(h.ctx, "replay: scenarioSystemHaltedRefusesQuoteAndOrder"); err != nil {
			return fmt.Errorf("halting C1: %w", err)
		}
		defer func() { _ = ledgerHalt.clear(h.ctx, "replay: scenario complete") }()

		status, _ := h.doJSON(http.MethodPost, "/v1/quotes", rawKey, "",
			map[string]any{"tier": "STANDARD", "amount_in": "3000.000000", "recipient_address": "TReplayRecipient"})
		if status != http.StatusLocked {
			return fmt.Errorf("POST /v1/quotes while halted: status %d, want 423", status)
		}

		externalID := h.uniqueExternalID("halted")
		status, _ = h.createOrder(rawKey, quote.QuoteID, externalID, "replay:halted:"+externalID)
		if status != http.StatusLocked {
			return fmt.Errorf("POST /v1/orders while halted: status %d, want 423", status)
		}

		if _, err := h.ledger.getOrder(h.ctx, externalID); err == nil {
			return fmt.Errorf("C1 has an order for %s despite the halt refusing order creation", externalID)
		}
		return nil
	})
}

// scenarioAllFourSandboxTriggersIsolated is the SCENARIO MIX's seventh
// row: all four sandbox triggers, run in the same harness, confirmed
// isolated from the production scenarios also running.
func (h *harness) scenarioAllFourSandboxTriggersIsolated() Result {
	const name = "AllFourSandboxTriggersIsolated"
	return runGuarded(name, func() error {
		_, rawKey, err := h.customersStore.CreateSandbox(h.ctx, "replay-sandbox-co")
		if err != nil {
			return err
		}
		_, prodRawKey, err := h.customersStore.Create(h.ctx, "replay-sandbox-isolation-prod-co")
		if err != nil {
			return err
		}

		for _, trigger := range []string{"reorg", "screening_hold", "energy_exhaustion", "retry_storm"} {
			status, body := h.doJSON(http.MethodPost, "/v1/sandbox/orders", rawKey, "",
				map[string]any{"tier": "STANDARD", "amount_in": "3000.000000", "recipient_address": "TReplayRecipient", "trigger": trigger})
			if status != http.StatusCreated {
				return fmt.Errorf("POST /v1/sandbox/orders trigger=%s: status %d: %s", trigger, status, body)
			}
			var resp map[string]any
			json.Unmarshal(body, &resp)
			externalID, _ := resp["external_id"].(string)
			if !strings.HasPrefix(externalID, "sbx_") {
				return fmt.Errorf("sandbox external_id %q missing sbx_ prefix", externalID)
			}
			h.sandboxExternalIDs = append(h.sandboxExternalIDs, externalID)

			// Invisible to the production status endpoint, even queried
			// by a real production customer's own key.
			prodStatus, _ := h.getOrderStatus(prodRawKey, externalID)
			if prodStatus != http.StatusNotFound {
				return fmt.Errorf("production GET /v1/orders/%s: status %d, want 404", externalID, prodStatus)
			}
		}
		return nil
	})
}

// --- misc helpers -----------------------------------------------------

const webhooksDefaultMaxAttempts = 8

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func jsonBody(r *http.Request) ([]byte, error) {
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(r.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ledgerHaltFixture drives C1's own halt control surface directly --
// harness-only, an operator action gateway itself never performs.
type ledgerHaltFixture struct {
	baseURL, token string
}

func (f *ledgerHaltFixture) set(ctx context.Context, reason string) error {
	return f.post(ctx, map[string]any{"action": "set", "reason": reason})
}
func (f *ledgerHaltFixture) clear(ctx context.Context, note string) error {
	return f.post(ctx, map[string]any{"action": "clear", "note": note})
}
func (f *ledgerHaltFixture) post(ctx context.Context, body map[string]any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.baseURL+"/v1/system/halt", bytes.NewReader(mustJSON(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Idempotency-Key", "replay:halt-toggle")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST /v1/system/halt: status %d", resp.StatusCode)
	}
	return nil
}

type webhookDeliveryRow struct {
	deliveredAt  bool
	attemptCount int
}

func findWebhookDelivery(ctx context.Context, pool *db.Pool, externalID, eventType string) (webhookDeliveryRow, error) {
	var deliveredAt *time.Time
	var attemptCount int
	row := pool.QueryRow(ctx, `SELECT delivered_at, attempt_count FROM webhook_deliveries WHERE external_id = $1 AND event_type = $2`, externalID, eventType)
	if err := row.Scan(&deliveredAt, &attemptCount); err != nil {
		return webhookDeliveryRow{}, fmt.Errorf("reading webhook_deliveries for %s/%s: %w", externalID, eventType, err)
	}
	return webhookDeliveryRow{deliveredAt: deliveredAt != nil, attemptCount: attemptCount}, nil
}
