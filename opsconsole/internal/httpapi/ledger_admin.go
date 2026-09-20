package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"opsconsole/internal/opclient"
)

// This file closes "Read this fourth"'s first named gap in
// docs/03-build/admin-panel-build-prompts.md: C1's own journal, trial
// balance, and manual reversal/reorg/forced-transition surface had no
// page at all. It carries the highest blast radius of any file in this
// console -- a reversal, a reorg, or a forced transition writes directly
// against the system of record -- so every write handler here uses a
// stronger confirmation than the checkbox-or-typed-field pattern every
// other write in this console uses, and forced transitions get their
// own, visibly distinct audit action type.
//
// Two real gaps against the document's own assumptions, found by
// reading ledger/internal/httpapi/server.go directly rather than
// trusting the document's route list:
//
//  1. There is no GET /v1/entries list route. The real route
//     (ledger/internal/httpapi/reversals.go's own getEntryByKey) is
//     GET /v1/entries?idempotency_key=... -- a lookup by a key the
//     caller already has, not a paginated browse-everything list. C1's
//     own journal has no "show me every entry" capability today, over
//     HTTP, for anyone. This page offers lookup by id or by
//     idempotency_key, matching what C1 actually supports, rather than
//     faking pagination over a list that doesn't exist.
//  2. There is no GET /v1/reconciliation/snapshots route -- only
//     POST /v1/reconciliation/snapshots (create one) exists; nothing
//     reads snapshots back over HTTP. Per this document's own
//     completeness-bar policy (name a real capability gap rather than
//     fake a UI over it), that page says so rather than showing an
//     empty list that implies snapshots were looked for and none exist.
//
// There is also no separate GET /v1/accounts route -- GET /v1/balances
// with an empty prefix already returns every account's balance in one
// call (ledger/internal/httpapi/balances.go's own getBalances, built on
// accounts.List with an unfiltered CodePrefix), so that alone covers
// "overall balance of each part of the product" without a second route.

const ledgerTabs = `
<div class="tabs">
  <a class="tab {{ if eq .LedgerTab "halt" }}active{{ end }}" href="/ledger/halt">Halt</a>
  <a class="tab {{ if eq .LedgerTab "balances" }}active{{ end }}" href="/ledger/balances">Balances</a>
  <a class="tab {{ if eq .LedgerTab "trial-balance" }}active{{ end }}" href="/ledger/trial-balance">Trial balance</a>
  <a class="tab {{ if eq .LedgerTab "entries" }}active{{ end }}" href="/ledger/entries">Entries</a>
  <a class="tab {{ if eq .LedgerTab "reconciliation" }}active{{ end }}" href="/ledger/reconciliation-snapshots">Reconciliation</a>
  <a class="tab {{ if eq .LedgerTab "reorg" }}active{{ end }}" href="/ledger/orders/reorg">Reorg</a>
  <a class="tab {{ if eq .LedgerTab "transition" }}active{{ end }}" href="/ledger/orders/transition">Force transition</a>
</div>
`

const ledgerBalancesContent = `
<div class="page-head"><h1>Account balances</h1></div>
` + ledgerTabs + `
<p class="helptext">Every account C1 knows about and its current balance -- GET /v1/balances with no prefix, C1's own account list and balance list in one call (there is no separate GET /v1/accounts route). Grouped by the account-code prefix before the first colon, matching the chart of accounts' own grouping.</p>
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
{{ range .Groups }}
<div class="card section">
  <div class="card-head"><h2 style="font-size:14px;text-transform:capitalize">{{ .Prefix }}</h2></div>
  <div class="table-wrap">
  <table>
  <tr><th>Account code</th><th>Asset</th><th>Balance</th></tr>
  {{ range .Rows }}
  <tr><td class="mono">{{ .AccountCode }}</td><td>{{ .Asset }}</td><td class="mono">{{ .Balance }}</td></tr>
  {{ end }}
  </table>
  </div>
</div>
{{ else }}
<div class="empty-state">C1 has no accounts yet.</div>
{{ end }}
`

const ledgerTrialBalanceContent = `
<div class="page-head"><h1>Trial balance</h1></div>
` + ledgerTabs + `
<p class="helptext">GET /v1/trial-balance -- the sum across every account, by asset. A balanced ledger sums to zero for every asset; anything else is a real integrity problem, not a display bug.</p>
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
<div class="table-wrap section">
<table>
<tr><th>Asset</th><th>Total</th></tr>
{{ range .Totals }}
<tr><td>{{ .Asset }}</td><td class="mono">{{ .Total }}</td></tr>
{{ else }}
<tr><td colspan="2"><div class="empty-state">No entries posted yet.</div></td></tr>
{{ end }}
</table>
</div>
`

const ledgerEntriesContent = `
<div class="page-head"><h1>Journal entries</h1></div>
` + ledgerTabs + `
<p class="helptext">C1 has no route to browse every entry -- GET /v1/entries requires an idempotency_key you already have. Look an entry up by its numeric id (e.g. from an order detail's own dispatch/reservation trail) or by the idempotency_key its producer used.</p>
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
<div class="card panel-narrow">
  <form method="get" action="/ledger/entries">
    <label>Entry id</label>
    <input type="text" name="id" value="{{ .QueryID }}" placeholder="42">
    <label style="margin-top:10px">or idempotency_key</label>
    <input type="text" name="key" value="{{ .QueryKey }}" placeholder="ledger:dispatch:...">
    <p style="margin-top:16px"><button class="btn btn-primary" type="submit">Look up</button></p>
  </form>
</div>
{{ if .Entry }}
<div class="card section">
  <div class="card-head">
    <h2 style="font-size:14px">Entry {{ .Entry.ID }}</h2>
    {{ if .Entry.ReversalOf }}<span class="badge badge-warning" style="margin-left:auto">reversal of {{ .Entry.ReversalOf }}</span>{{ end }}
  </div>
  <div class="fact"><span class="fact-label">idempotency_key</span><span class="fact-value mono">{{ .Entry.IdempotencyKey }}</span></div>
  <div class="fact"><span class="fact-label">entry_type</span><span class="fact-value">{{ .Entry.EntryType }}</span></div>
  <div class="fact"><span class="fact-label">order_id</span><span class="fact-value">{{ if .Entry.OrderID }}{{ .Entry.OrderID }}{{ else }}—{{ end }}</span></div>
  <div class="fact"><span class="fact-label">actor</span><span class="fact-value">{{ .Entry.Actor }}</span></div>
  <div class="fact"><span class="fact-label">occurred_at</span><span class="fact-value mono">{{ .Entry.OccurredAt }}</span></div>
  <div class="fact"><span class="fact-label">recorded_at</span><span class="fact-value mono">{{ .Entry.RecordedAt }}</span></div>
  <div class="fact"><span class="fact-label">outcome</span><span class="fact-value">{{ .Entry.Outcome }}</span></div>
  <div class="table-wrap" style="margin-top:12px">
  <table>
  <tr><th>Seq</th><th>Account</th><th>Asset</th><th>Amount</th></tr>
  {{ range .Entry.Lines }}
  <tr><td class="mono">{{ .Seq }}</td><td class="mono">{{ .AccountCode }}</td><td>{{ .Asset }}</td><td class="mono">{{ .Amount }}</td></tr>
  {{ end }}
  </table>
  </div>
  {{ if not .Entry.ReversalOf }}
  <details style="margin-top:14px">
    <summary style="cursor:pointer;color:var(--danger)">Reverse this entry</summary>
    <form method="post" action="/ledger/entries/{{ .Entry.ID }}/reversal" style="margin-top:12px">
      <label>reason (required, written to the audit log verbatim)</label>
      <input type="text" name="reason" required>
      <label style="margin-top:10px">type <span class="mono">{{ .Entry.ID }}</span> to confirm</label>
      <input type="text" name="confirm_id" required placeholder="{{ .Entry.ID }}">
      <p style="margin-top:16px"><button class="btn btn-danger" type="submit">Reverse entry {{ .Entry.ID }}</button></p>
    </form>
  </details>
  {{ end }}
</div>
{{ end }}
`

const ledgerReconciliationContent = `
<div class="page-head"><h1>Reconciliation snapshots</h1></div>
` + ledgerTabs + `
<div class="card section">
  <p class="field-hint" style="margin:0">not available. C1's only real route here is POST /v1/reconciliation/snapshots (create a snapshot) -- there is no GET route that reads one back over HTTP, for anyone, today. Adding one is a real C1 gap this document names rather than papers over with a fake empty list.</p>
</div>
`

const ledgerReorgContent = `
<div class="page-head"><h1>Report a deposit reorg</h1></div>
` + ledgerTabs + `
<p class="helptext">POST /v1/orders/{external_id}/reorg reports one fact -- the deposit that funded this order no longer exists on chain -- and C1 alone decides what that means from the order's own current state (reverse to quoted with no loss, or reverse and book a loss and halt, if the payout already left). It takes no scenario and no target state; only original_entry_key, the idempotency_key of the deposit_final entry that funded the order, the same key the watcher that detected the reorg already has.</p>
{{ if .Flash }}<div class="flash {{ if .FlashError }}flash-error{{ else }}flash-ok{{ end }}">{{ if .FlashError }}` + iconAlert + `{{ else }}` + iconCheck + `{{ end }}<span>{{ .Flash }}</span></div>{{ end }}
{{ if .QueryExternalID }}
<div class="card panel-narrow">
{{ if .Order.ExternalID }}<p class="field-hint">Order {{ .Order.ExternalID }} is currently <span class="badge {{ .StateBadge }}">{{ .Order.State }}</span>.</p>{{ end }}
<form method="post" action="/ledger/orders/{{ .QueryExternalID }}/reorg">
  <label>external_id</label>
  <input type="text" name="external_id_display" value="{{ .QueryExternalID }}" disabled>
  <label style="margin-top:10px">original_entry_key</label>
  <input type="text" name="original_entry_key" required placeholder="the funding deposit's own idempotency_key">
  <label style="margin-top:10px">type <span class="mono">{{ .QueryExternalID }}</span> to confirm</label>
  <input type="text" name="confirm_id" required placeholder="{{ .QueryExternalID }}">
  <p style="margin-top:16px"><button class="btn btn-danger" type="submit">Report reorg</button></p>
</form>
</div>
{{ end }}
<div class="card panel-narrow" style="margin-top:16px">
  <form method="get" action="/ledger/orders/reorg">
    <label>{{ if .QueryExternalID }}Look up a different order{{ else }}Look up an order{{ end }}</label>
    <input type="text" name="external_id" placeholder="order external_id" required>
    <p style="margin-top:16px"><button class="btn btn-sm" type="submit">Look up</button></p>
  </form>
</div>
`

const ledgerTransitionContent = `
<div class="page-head"><h1>Force an order transition</h1></div>
` + ledgerTabs + `
<div class="flash flash-error">` + iconAlert + `<span>The highest-risk action in this entire console. C1's own transition table still rejects an illegal (from, to) pair -- this is not a bypass of that. It IS a bypass of every OTHER service's own business reason for making a transition: C5 normally drives screened -&gt; dispatching only after a real energy reservation and S1 signature exist, and this button can post that transition with none of that having happened.</span></div>
{{ if .Flash }}<div class="flash {{ if .FlashError }}flash-error{{ else }}flash-ok{{ end }}">{{ if .FlashError }}` + iconAlert + `{{ else }}` + iconCheck + `{{ end }}<span>{{ .Flash }}</span></div>{{ end }}
{{ if .QueryExternalID }}
<div class="card panel-narrow">
{{ if .Order.ExternalID }}<p class="field-hint">Order {{ .Order.ExternalID }} is currently <span class="badge {{ .StateBadge }}">{{ .Order.State }}</span>, version {{ .Order.Version }}.</p>{{ end }}
<form method="post" action="/ledger/orders/{{ .QueryExternalID }}/transitions">
  <label>external_id</label>
  <input type="text" name="external_id_display" value="{{ .QueryExternalID }}" disabled>
  <label style="margin-top:10px">to_state</label>
  <select name="to_state" required>
    {{ range .States }}<option value="{{ . }}">{{ . }}</option>{{ end }}
  </select>
  <label style="margin-top:10px">expected_version</label>
  <input type="number" name="expected_version" value="{{ .Order.Version }}" required>
  <label style="margin-top:10px">reason (required, written to the audit log verbatim)</label>
  <input type="text" name="reason" required>
  <label style="margin-top:10px">entry_id to cite as cause <span style="font-weight:400">(optional -- e.g. a reversal already posted above; this form does not construct new journal lines)</span></label>
  <input type="text" name="entry_id" placeholder="optional">
  <label style="margin-top:10px">type <span class="mono">I understand this skips C2/C3/C4/C5's own checks</span> to confirm</label>
  <input type="text" name="confirm_phrase" required placeholder="I understand this skips C2/C3/C4/C5's own checks">
  <p style="margin-top:16px"><button class="btn btn-danger" type="submit">Force transition</button></p>
</form>
</div>
{{ end }}
<div class="card panel-narrow" style="margin-top:16px">
  <form method="get" action="/ledger/orders/transition">
    <label>{{ if .QueryExternalID }}Look up a different order{{ else }}Look up an order{{ end }}</label>
    <input type="text" name="external_id" placeholder="order external_id" required>
    <p style="margin-top:16px"><button class="btn btn-sm" type="submit">Look up</button></p>
  </form>
</div>
`

const transitionConfirmPhrase = "I understand this skips C2/C3/C4/C5's own checks"

type balanceGroup struct {
	Prefix string
	Rows   []opclient.BalanceEntry
}

type ledgerBalancesPageData struct {
	basePageData
	LedgerTab string
	Groups    []*balanceGroup
	Error     string
}

// getLedgerBalances builds OC.16's "overall balance of each part of the
// product" view from C1's real GET /v1/balances with no prefix -- see
// this file's own doc comment for why that alone covers "list every
// account" too.
func (s *Server) getLedgerBalances(w http.ResponseWriter, r *http.Request) {
	data := ledgerBalancesPageData{basePageData: s.newBasePageData(r), LedgerTab: "balances"}
	rows, err := s.Ledger.GetBalances(r.Context(), "")
	if err != nil {
		data.Error = "reading balances: " + err.Error()
		s.Templates.Render(w, "ledger_balances", data)
		return
	}
	byPrefix := map[string]*balanceGroup{}
	for _, row := range rows {
		prefix, _, _ := strings.Cut(row.AccountCode, ":")
		g, ok := byPrefix[prefix]
		if !ok {
			g = &balanceGroup{Prefix: prefix}
			byPrefix[prefix] = g
			// account codes come back from C1 already ordered, so
			// appending on first sight of each prefix keeps groups in
			// that same stable order (asset:, liability:, ...) with no
			// second sort needed.
			data.Groups = append(data.Groups, g)
		}
		g.Rows = append(g.Rows, row)
	}
	s.Templates.Render(w, "ledger_balances", data)
}

type trialBalanceRow struct{ Asset, Total string }

type ledgerTrialBalancePageData struct {
	basePageData
	LedgerTab string
	Totals    []trialBalanceRow
	Error     string
}

type ledgerReconciliationPageData struct {
	basePageData
	LedgerTab string
}

// getLedgerReconciliation names a real C1 gap rather than faking a read
// -- see this file's own doc comment, item 2.
func (s *Server) getLedgerReconciliation(w http.ResponseWriter, r *http.Request) {
	s.Templates.Render(w, "ledger_reconciliation", ledgerReconciliationPageData{basePageData: s.newBasePageData(r), LedgerTab: "reconciliation"})
}

func (s *Server) getLedgerTrialBalance(w http.ResponseWriter, r *http.Request) {
	data := ledgerTrialBalancePageData{basePageData: s.newBasePageData(r), LedgerTab: "trial-balance"}
	totals, err := s.Ledger.GetTrialBalance(r.Context())
	if err != nil {
		data.Error = "reading trial balance: " + err.Error()
		s.Templates.Render(w, "ledger_trial_balance", data)
		return
	}
	for asset, total := range totals {
		data.Totals = append(data.Totals, trialBalanceRow{Asset: asset, Total: total})
	}
	s.Templates.Render(w, "ledger_trial_balance", data)
}

type entryLineView struct {
	Seq                        int
	AccountCode, Asset, Amount string
}

type entryView struct {
	ID                                                                int64
	IdempotencyKey, EntryType, Actor, OccurredAt, RecordedAt, Outcome string
	OrderID, ReversalOf                                               *int64
	Lines                                                             []entryLineView
}

type ledgerEntriesPageData struct {
	basePageData
	LedgerTab         string
	QueryID, QueryKey string
	Entry             *entryView
	Error             string
}

func toEntryView(e opclient.EntryDetail) *entryView {
	v := &entryView{
		ID: e.ID, IdempotencyKey: e.IdempotencyKey, EntryType: e.EntryType, OrderID: e.OrderID,
		Actor: e.Actor, OccurredAt: e.OccurredAt.String(), RecordedAt: e.RecordedAt.String(),
		ReversalOf: e.ReversalOf, Outcome: e.Outcome,
	}
	for _, l := range e.Lines {
		v.Lines = append(v.Lines, entryLineView{Seq: l.Seq, AccountCode: l.AccountCode, Asset: l.Asset, Amount: l.Amount})
	}
	return v
}

// getLedgerEntries is GET /ledger/entries?id=... or ?key=... -- see this
// file's own doc comment on why this is a lookup form, not a list.
func (s *Server) getLedgerEntries(w http.ResponseWriter, r *http.Request) {
	idParam := r.URL.Query().Get("id")
	keyParam := r.URL.Query().Get("key")
	data := ledgerEntriesPageData{basePageData: s.newBasePageData(r), LedgerTab: "entries", QueryID: idParam, QueryKey: keyParam}

	switch {
	case idParam != "":
		id, err := strconv.ParseInt(idParam, 10, 64)
		if err != nil {
			data.Error = "id must be an integer"
			break
		}
		entry, err := s.Ledger.GetEntry(r.Context(), id)
		if err != nil {
			data.Error = "looking up entry " + idParam + ": " + err.Error()
			break
		}
		data.Entry = toEntryView(entry)
	case keyParam != "":
		entry, err := s.Ledger.GetEntryByKey(r.Context(), keyParam)
		if err != nil {
			data.Error = "looking up entry by key: " + err.Error()
			break
		}
		data.Entry = toEntryView(entry)
	}
	s.Templates.Render(w, "ledger_entries", data)
}

// postLedgerEntryReversal calls C1's own real POST
// /v1/entries/{id}/reversal. reason is required and rejected empty
// server-side before any downstream call -- the client-side `required`
// attribute is only a courtesy, per this chunk's own acceptance bar.
// confirm_id must equal the entry's own id exactly, typed by the
// operator, before this handler calls C1 at all.
func (s *Server) postLedgerEntryReversal(w http.ResponseWriter, r *http.Request) {
	idParam := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idParam, 10, 64)
	if err != nil {
		http.Error(w, "invalid entry id", http.StatusBadRequest)
		return
	}
	renderErr := func(msg string) {
		data := ledgerEntriesPageData{basePageData: s.newBasePageData(r), LedgerTab: "entries", QueryID: idParam, Error: msg}
		if entry, err := s.Ledger.GetEntry(r.Context(), id); err == nil {
			data.Entry = toEntryView(entry)
		}
		s.Templates.Render(w, "ledger_entries", data)
	}
	if err := r.ParseForm(); err != nil {
		renderErr("malformed form submission")
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		renderErr("reason is required")
		return
	}
	if r.FormValue("confirm_id") != idParam {
		renderErr("confirmation did not match entry id " + idParam + " -- reversal was not attempted")
		return
	}

	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "ledger.entry.reversal", "entry:"+idParam, map[string]any{"reason": reason})

	if _, err := s.Ledger.PostReversal(r.Context(), id, reason, time.Time{}); err != nil {
		renderErr("reversing entry " + idParam + ": " + err.Error())
		return
	}
	http.Redirect(w, r, "/ledger/entries?id="+idParam, http.StatusFound)
}

type ledgerOrderActionPageData struct {
	basePageData
	LedgerTab       string
	QueryExternalID string
	Order           opclient.Order
	StateBadge      string
	States          []string
	Flash           string
	FlashError      bool
}

func (s *Server) getLedgerReorgForm(w http.ResponseWriter, r *http.Request) {
	externalID := r.URL.Query().Get("external_id")
	data := ledgerOrderActionPageData{basePageData: s.newBasePageData(r), LedgerTab: "reorg", QueryExternalID: externalID}
	if externalID != "" {
		if order, err := s.Ledger.GetOrder(r.Context(), externalID); err == nil {
			data.Order = order
			data.StateBadge = stateBadgeClass(order.State)
		} else {
			data.FlashError = true
			data.Flash = "looking up " + externalID + ": " + err.Error()
		}
	}
	s.Templates.Render(w, "ledger_reorg", data)
}

// postLedgerReorg calls C1's own real POST /v1/orders/{external_id}/reorg.
// confirm_id must equal external_id exactly before this handler calls C1
// at all -- C1 itself takes no confirmation field on this route (see
// opclient.PostReorg's own doc comment), so this console adds one.
func (s *Server) postLedgerReorg(w http.ResponseWriter, r *http.Request) {
	externalID := chi.URLParam(r, "external_id")
	render := func(flash string, flashErr bool) {
		data := ledgerOrderActionPageData{basePageData: s.newBasePageData(r), LedgerTab: "reorg", QueryExternalID: externalID, Flash: flash, FlashError: flashErr}
		if order, err := s.Ledger.GetOrder(r.Context(), externalID); err == nil {
			data.Order = order
			data.StateBadge = stateBadgeClass(order.State)
		}
		s.Templates.Render(w, "ledger_reorg", data)
	}
	if err := r.ParseForm(); err != nil {
		render("malformed form submission", true)
		return
	}
	originalEntryKey := strings.TrimSpace(r.FormValue("original_entry_key"))
	if originalEntryKey == "" {
		render("original_entry_key is required", true)
		return
	}
	if r.FormValue("confirm_id") != externalID {
		render("confirmation did not match order "+externalID+" -- reorg was not reported", true)
		return
	}

	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "ledger.order.reorg", "order:"+externalID, map[string]any{"original_entry_key": originalEntryKey})

	if _, err := s.Ledger.PostReorg(r.Context(), externalID, originalEntryKey); err != nil {
		render("reporting reorg for "+externalID+": "+err.Error(), true)
		return
	}
	render(externalID+": reorg reported.", false)
}

func (s *Server) getLedgerTransitionForm(w http.ResponseWriter, r *http.Request) {
	externalID := r.URL.Query().Get("external_id")
	data := ledgerOrderActionPageData{basePageData: s.newBasePageData(r), LedgerTab: "transition", QueryExternalID: externalID, States: orderStates}
	if externalID != "" {
		if order, err := s.Ledger.GetOrder(r.Context(), externalID); err == nil {
			data.Order = order
			data.StateBadge = stateBadgeClass(order.State)
		} else {
			data.FlashError = true
			data.Flash = "looking up " + externalID + ": " + err.Error()
		}
	}
	s.Templates.Render(w, "ledger_transition", data)
}

// postLedgerTransition calls C1's own real POST
// /v1/orders/{external_id}/transitions -- deliberately narrower than
// what that real route accepts; see opclient.TransitionRequest's own
// doc comment on why this console never constructs freeform journal
// lines here. reason is required (rejected empty server-side, same as
// reversal above) and confirm_phrase must equal
// transitionConfirmPhrase exactly -- a different, harder-to-fat-finger
// string than the typed-id pattern every other dangerous action in this
// file uses, because this is this document's own named highest-risk
// action. The audit entry uses action "DANGEROUS_MANUAL_TRANSITION",
// deliberately not lowercase-dotted like every other action in this
// console's audit trail, so it stands out to anyone scanning the log
// without reading full JSON detail.
func (s *Server) postLedgerTransition(w http.ResponseWriter, r *http.Request) {
	externalID := chi.URLParam(r, "external_id")
	render := func(flash string, flashErr bool) {
		data := ledgerOrderActionPageData{basePageData: s.newBasePageData(r), LedgerTab: "transition", QueryExternalID: externalID, States: orderStates, Flash: flash, FlashError: flashErr}
		if order, err := s.Ledger.GetOrder(r.Context(), externalID); err == nil {
			data.Order = order
			data.StateBadge = stateBadgeClass(order.State)
		}
		s.Templates.Render(w, "ledger_transition", data)
	}
	if err := r.ParseForm(); err != nil {
		render("malformed form submission", true)
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		render("reason is required", true)
		return
	}
	if r.FormValue("confirm_phrase") != transitionConfirmPhrase {
		render("confirmation phrase did not match exactly -- transition was not attempted", true)
		return
	}
	toState := r.FormValue("to_state")
	expectedVersion, err := strconv.ParseInt(r.FormValue("expected_version"), 10, 32)
	if err != nil {
		render("expected_version must be an integer", true)
		return
	}

	req := opclient.TransitionRequest{ToState: toState, ExpectedVersion: int32(expectedVersion), Reason: reason}
	if raw := strings.TrimSpace(r.FormValue("entry_id")); raw != "" {
		entryID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			render("entry_id must be an integer", true)
			return
		}
		req.EntryID = &entryID
	}

	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "DANGEROUS_MANUAL_TRANSITION", "order:"+externalID, map[string]any{
		"to_state": toState, "expected_version": expectedVersion, "reason": reason, "entry_id": req.EntryID, "operator": sess.DisplayName,
	})

	if _, err := s.Ledger.PostTransition(r.Context(), externalID, req); err != nil {
		render("forcing transition for "+externalID+": "+err.Error(), true)
		return
	}
	render(externalID+": forced to "+toState+".", false)
}
