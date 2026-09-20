package httpapi

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

const payoutsOutstandingContent = `
<div class="page-head"><h1>Outstanding payouts</h1></div>
<p class="helptext">
  "One click" starts the exact same real flow every other payout goes through --
  screening, energy reservation, and S1 approval are never bypassed by this page.
  Dispatch calls C5's own real <span class="mono">POST /v1/dispatch</span>, the
  identical call C5's own orchestrate loop makes; if this payout crosses S1's
  approval threshold it lands in the same
  <a href="/s1/approvals">S1 &gt; Approvals</a> queue any other payout uses.
</p>
{{ if .Flash }}<div class="flash {{ if .FlashError }}flash-error{{ else }}flash-ok{{ end }}">{{ if .FlashError }}` + iconAlert + `{{ else }}` + iconCheck + `{{ end }}<span>{{ .Flash }}</span></div>{{ end }}
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}

<div class="card section">
  <div class="card-head"><h2 style="font-size:14px">Screened, not yet dispatched</h2></div>
  {{ if .ScreenedEmptyNote }}<p class="field-hint" style="margin:0 0 10px">{{ .ScreenedEmptyNote }}</p>{{ end }}
  <div class="table-wrap">
  <table>
  <tr><th>Order</th><th>Customer</th><th>Amount in</th><th>Updated</th><th></th></tr>
  {{ range .Screened }}
  <tr>
    <td class="mono"><a href="/orders/{{ .ExternalID }}">{{ .ExternalID }}</a></td>
    <td>{{ .CustomerID }}</td><td class="mono">{{ .AmountIn }}</td><td class="mono">{{ .UpdatedAt }}</td>
    <td>
      <form class="inline" method="post" action="/payouts/{{ .ExternalID }}/dispatch">
        <div class="actions">
          <label style="font-weight:400"><input type="checkbox" name="confirm" value="true" required style="width:auto;vertical-align:middle"> confirm</label>
          <button class="btn btn-sm btn-primary" type="submit">Dispatch</button>
        </div>
      </form>
    </td>
  </tr>
  {{ else }}
  <tr><td colspan="5"><div class="empty-state">No screened orders waiting on dispatch right now.</div></td></tr>
  {{ end }}
  </table>
  </div>
</div>

<div class="card section">
  <div class="card-head"><h2 style="font-size:14px">Dispatching, failed or stalled attempt</h2></div>
  <p class="field-hint" style="margin:0 0 10px">
    Visibility only -- there is no real route to retry a dispatch while an order is
    still in <span class="mono">dispatching</span> (C5's own <span class="mono">POST /v1/dispatch</span>
    requires <span class="mono">screened</span> and rejects anything else). A FAILED attempt is picked
    up by C5's own periodic reconciler, which reverses the conversion and returns the order to
    <span class="mono">held</span> on its own -- from there it re-enters screening normally and will
    reappear in the list above once it reaches <span class="mono">screened</span> again. "Stalled" means
    no attempt has reached CONFIRMED within {{ .StuckMinutes }} minutes of entering dispatching, the
    same threshold <a href="/alerts">Alerts</a> uses. An order in dispatching with no dispatch record at
    all is a different, narrower gap already covered by <a href="/alerts">Alerts</a>, not repeated here.
  </p>
  <div class="table-wrap">
  <table>
  <tr><th>Order</th><th>Customer</th><th>Amount in</th><th>Entered dispatching</th><th>Attempt status</th></tr>
  {{ range .Stalled }}
  <tr>
    <td class="mono"><a href="/orders/{{ .ExternalID }}">{{ .ExternalID }}</a></td>
    <td>{{ .CustomerID }}</td><td class="mono">{{ .AmountIn }}</td><td class="mono">{{ .EnteredDispatchingAt }}</td>
    <td><span class="badge {{ if eq .AttemptStatus "FAILED" }}badge-danger{{ else }}badge-warning{{ end }}">{{ if .AttemptStatus }}{{ .AttemptStatus }}{{ else }}no attempt yet{{ end }}</span></td>
  </tr>
  {{ else }}
  <tr><td colspan="5"><div class="empty-state">No dispatching orders are failed or stalled right now.</div></td></tr>
  {{ end }}
  </table>
  </div>
</div>
`

type payoutOrderRow struct {
	ExternalID, CustomerID, AmountIn, UpdatedAt string
}

type stalledDispatchRow struct {
	ExternalID, CustomerID, AmountIn, EnteredDispatchingAt, AttemptStatus string
}

type payoutsPageData struct {
	basePageData
	Screened          []payoutOrderRow
	ScreenedEmptyNote string
	Stalled           []stalledDispatchRow
	StuckMinutes      int
	Flash             string
	FlashError        bool
	Error             string
}

// getPayoutsOutstanding builds OC.15's two lists entirely from real
// reads: C1's own GET /v1/orders?state=screened, and, for every order
// GET /v1/orders?state=dispatching, C5's own GET /v1/dispatch/{id} to
// find a FAILED or stalled latest_attempt_status. See this file's own
// helptext for why the second list carries no action button -- C5's
// real POST /v1/dispatch only accepts a screened order.
func (s *Server) getPayoutsOutstanding(w http.ResponseWriter, r *http.Request) {
	stuckMinutes := s.StuckOrderMinutes
	if stuckMinutes <= 0 {
		stuckMinutes = 30
	}
	data := payoutsPageData{basePageData: s.newBasePageData(r), StuckMinutes: stuckMinutes}
	s.renderPayoutsOutstanding(w, r, &data)
}

func (s *Server) renderPayoutsOutstanding(w http.ResponseWriter, r *http.Request, data *payoutsPageData) {
	ctx := r.Context()

	screened, err := s.Ledger.ListOrders(ctx, "screened", "", 200)
	if err != nil {
		data.Error = "listing screened orders: " + err.Error()
		s.Templates.Render(w, "payouts_outstanding", data)
		return
	}
	for _, o := range screened.Orders {
		data.Screened = append(data.Screened, payoutOrderRow{ExternalID: o.ExternalID, CustomerID: o.CustomerID, AmountIn: o.AmountIn, UpdatedAt: o.UpdatedAt})
	}
	if len(data.Screened) == 0 {
		data.ScreenedEmptyNote = "Normally empty: C5's own orchestrate loop picks up a screened order for dispatch fast enough that this list rarely holds anything for long."
	}

	dispatching, err := s.Ledger.ListOrders(ctx, "dispatching", "", 200)
	if err != nil {
		data.Error = "listing dispatching orders: " + err.Error()
		s.Templates.Render(w, "payouts_outstanding", data)
		return
	}
	cutoff := nowUTC().Add(-time.Duration(data.StuckMinutes) * time.Minute)
	for _, o := range dispatching.Orders {
		dis, err := s.Dispatcher.GetDispatch(ctx, o.ID)
		if err != nil {
			continue // no dispatch record at all -- Alerts' own orphaned-order check, not this list's job
		}
		row := stalledDispatchRow{ExternalID: o.ExternalID, CustomerID: o.CustomerID, AmountIn: o.AmountIn, EnteredDispatchingAt: dis.EnteredDispatchingAt.String()}
		switch {
		case dis.LatestAttemptStatus != nil && *dis.LatestAttemptStatus == "FAILED":
			row.AttemptStatus = "FAILED"
		case dis.LatestAttemptStatus != nil && *dis.LatestAttemptStatus == "CONFIRMED":
			continue // done -- C1's own transition to settled just hasn't been observed here yet, not this list's concern
		case dis.EnteredDispatchingAt.Before(cutoff):
			if dis.LatestAttemptStatus != nil {
				row.AttemptStatus = *dis.LatestAttemptStatus
			}
		default:
			continue // recent and still legitimately in flight
		}
		data.Stalled = append(data.Stalled, row)
	}

	s.Templates.Render(w, "payouts_outstanding", data)
}

// postPayoutDispatch calls C5's own real POST /v1/dispatch -- the same
// call C5's own orchestrate loop makes for a screened order, with the
// same Idempotency-Key convention every opclient write uses. A terminal
// or otherwise non-screened order is rejected with dispatcher's own
// real error (e.g. a 409 "order is settled, not screened"), rendered
// unchanged -- this page invents no validation of its own.
func (s *Server) postPayoutDispatch(w http.ResponseWriter, r *http.Request) {
	externalID := chi.URLParam(r, "external_id")
	stuckMinutes := s.StuckOrderMinutes
	if stuckMinutes <= 0 {
		stuckMinutes = 30
	}
	render := func(flash string, flashErr bool) {
		data := payoutsPageData{basePageData: s.newBasePageData(r), StuckMinutes: stuckMinutes, Flash: flash, FlashError: flashErr}
		s.renderPayoutsOutstanding(w, r, &data)
	}
	if err := r.ParseForm(); err != nil || r.FormValue("confirm") != "true" {
		render("dispatch not confirmed -- check the confirm box before submitting", true)
		return
	}

	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "payout.dispatch", "order:"+externalID, map[string]any{"external_id": externalID})

	result, err := s.Dispatcher.Dispatch(r.Context(), externalID)
	if err != nil {
		render("dispatching "+externalID+": "+err.Error(), true)
		return
	}
	render(externalID+" entered dispatching on slot "+intStr(result.SlotID)+" -- watch S1 > Approvals if this payout needs sign-off.", false)
}
