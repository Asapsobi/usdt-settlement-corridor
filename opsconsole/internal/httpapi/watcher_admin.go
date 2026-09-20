package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"opsconsole/internal/opclient"
)

// This file closes the C2 (deposit watcher) routes no existing chunk
// (OC.4's cursor page, OC.11's sweep page, OC.12's deposit board) reaches:
// the full address inventory (including RETIRED, for wallet-registry
// completeness), retiring an address, orphaned-deposit review, and RPC
// provider health.

const watcherAddressesContent = `
<div class="page-head"><h1>Watcher addresses</h1></div>
` + watcherTabs + `
<p class="helptext">Every address C2 has ever assigned, every status -- unlike <a href="/deposits/watching">Deposits</a> (WATCHING/FUNDED only) or <a href="/watcher/sweep">Sweep</a> (BSC sweep workflow), this is the full inventory OC.11's wallet registry links out to.</p>
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
<div class="table-wrap">
<table>
<tr><th>Order</th><th>Address</th><th>Index</th><th>Status</th><th>Assigned</th><th>Retired</th><th></th></tr>
{{ range .Addresses }}
<tr>
  <td>{{ .OrderID }}</td>
  <td class="mono">{{ .Address }}</td>
  <td>{{ .DerivationIndex }}</td>
  <td><span class="badge {{ if eq .Status "RETIRED" }}badge-neutral{{ else if eq .Status "FUNDED" }}badge-success{{ else }}badge-warning{{ end }}">{{ .Status }}</span></td>
  <td class="mono">{{ .AssignedAt }}</td>
  <td>{{ if .RetiredAt }}<span class="mono">{{ .RetiredAt }}</span> ({{ .RetiredReason }}){{ end }}</td>
  <td>{{ if ne .Status "RETIRED" }}<a class="btn btn-sm" href="/watcher/addresses/{{ .OrderID }}/retire">Retire</a>{{ end }}</td>
</tr>
{{ else }}
<tr><td colspan="7"><div class="empty-state">No watched addresses yet.</div></td></tr>
{{ end }}
</table>
</div>
`

const watcherRetireContent = `
<div class="page-head"><h1>Retire address for order {{ .OrderID }}</h1></div>
` + watcherTabs + `
{{ if .Flash }}<div class="flash {{ if .FlashError }}flash-error{{ else }}flash-ok{{ end }}">{{ if .FlashError }}` + iconAlert + `{{ else }}` + iconCheck + `{{ end }}<span>{{ .Flash }}</span></div>{{ end }}
<div class="card panel-narrow">
{{ if .Address.Address }}<p class="field-hint">Address <span class="mono">{{ .Address.Address }}</span>, currently <span class="badge {{ if eq .Address.Status "FUNDED" }}badge-success{{ else }}badge-warning{{ end }}">{{ .Address.Status }}</span>.</p>{{ end }}
<div class="flash flash-error">` + iconAlert + `<span>Retirement is explicit and permanent: a retired address can never be reassigned or re-watched again (invariant 6 -- never reuse a derivation index or an address). This is not reversible from this console or any other.</span></div>
<form method="post" action="/watcher/addresses/{{ .OrderID }}/retire">
  <label>reason <span style="font-weight:400">(free text; by convention one of settled, refunded, expired, or superseded)</span></label>
  <select name="reason" required>
    <option value="">-- choose --</option>
    <option value="settled">settled</option>
    <option value="refunded">refunded</option>
    <option value="expired">expired</option>
    <option value="superseded">superseded</option>
  </select>
  <label style="margin-top:10px">type <span class="mono">{{ .OrderID }}</span> to confirm</label>
  <input type="text" name="confirm_id" required placeholder="{{ .OrderID }}">
  <p style="margin-top:16px"><button class="btn btn-danger" type="submit">Retire address</button></p>
</form>
</div>
`

const watcherOrphanedContent = `
<div class="page-head"><h1>Orphaned deposits</h1></div>
` + watcherTabs + `
<p class="helptext">A deposit C2 detected that doesn't cleanly match the normal crediting path (e.g. arrived after the order's own address was retired, or after quote expiry) -- recorded for manual review rather than silently dropped or silently credited.</p>
{{ if .Flash }}<div class="flash {{ if .FlashError }}flash-error{{ else }}flash-ok{{ end }}">{{ if .FlashError }}` + iconAlert + `{{ else }}` + iconCheck + `{{ end }}<span>{{ .Flash }}</span></div>{{ end }}
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
<div class="table-wrap">
<table>
<tr><th>ID</th><th>Order</th><th>Tx</th><th>Amount</th><th>Detected</th><th>Order state then</th><th>Resolution</th><th></th></tr>
{{ range .Deposits }}
<tr>
  <td class="mono">{{ .ID }}</td>
  <td class="mono"><a href="/orders/{{ .ExternalID }}">{{ .ExternalID }}</a></td>
  <td class="mono" style="max-width:160px;overflow:hidden;text-overflow:ellipsis">{{ .TxHash }}</td>
  <td class="mono">{{ .Amount }}</td>
  <td class="mono">{{ .DetectedAt }}</td>
  <td>{{ .OrderStateAtDetection }}</td>
  <td>{{ if .Resolution }}<span class="badge badge-success">{{ .Resolution }}</span>{{ else }}<span class="badge badge-warning">open</span>{{ end }}</td>
  <td>{{ if not .Resolution }}
    <form class="inline" method="post" action="/watcher/orphaned-deposits/{{ .ID }}/resolve">
      <div class="actions">
        <input type="text" name="resolution" placeholder="resolution (free text)" required style="width:200px">
        <button class="btn btn-sm" type="submit">Resolve</button>
      </div>
    </form>
  {{ end }}</td>
</tr>
{{ else }}
<tr><td colspan="8"><div class="empty-state">No orphaned deposits.</div></td></tr>
{{ end }}
</table>
</div>
`

const watcherProvidersContent = `
<div class="page-head"><h1>RPC providers</h1></div>
` + watcherTabs + `
<p class="helptext">GET /v1/system/providers -- read-only health of the chain RPC provider pool this watcher instance reads from.</p>
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
<div class="table-wrap">
<table>
<tr><th>Provider</th><th>Status</th><th>Consecutive failures</th><th>Total rounds</th><th>Total failures</th><th>Last error</th></tr>
{{ range .Providers }}
<tr>
  <td>{{ .Name }}</td>
  <td><span class="badge {{ if .Healthy }}badge-success{{ else }}badge-danger{{ end }}">{{ if .Healthy }}healthy{{ else }}unhealthy{{ end }}</span></td>
  <td class="mono">{{ .ConsecutiveFailures }}</td>
  <td class="mono">{{ .TotalRounds }}</td>
  <td class="mono">{{ .TotalFailures }}</td>
  <td>{{ if .LastError }}{{ .LastError }}{{ else }}—{{ end }}</td>
</tr>
{{ else }}
<tr><td colspan="6"><div class="empty-state">No providers configured on this instance.</div></td></tr>
{{ end }}
</table>
</div>
`

type watcherAddressRow struct {
	OrderID                  int64
	Address                  string
	DerivationIndex          uint32
	Status, AssignedAt       string
	RetiredAt, RetiredReason string
}

type watcherAddressesPageData struct {
	basePageData
	WatcherTab string
	Addresses  []watcherAddressRow
	Error      string
}

func (s *Server) getWatcherAddresses(w http.ResponseWriter, r *http.Request) {
	data := watcherAddressesPageData{basePageData: s.newBasePageData(r), WatcherTab: "addresses"}
	list, err := s.Watcher.ListAddresses(r.Context(), 500)
	if err != nil {
		data.Error = err.Error()
		s.Templates.Render(w, "watcher_addresses", data)
		return
	}
	for _, a := range list {
		row := watcherAddressRow{OrderID: a.OrderID, Address: a.Address, DerivationIndex: a.DerivationIndex, Status: a.Status, AssignedAt: a.AssignedAt.String()}
		if a.RetiredAt != nil {
			row.RetiredAt = a.RetiredAt.String()
		}
		if a.RetiredReason != nil {
			row.RetiredReason = *a.RetiredReason
		}
		data.Addresses = append(data.Addresses, row)
	}
	s.Templates.Render(w, "watcher_addresses", data)
}

type watcherRetirePageData struct {
	basePageData
	WatcherTab string
	OrderID    int64
	Address    watcherAddressRow
	Flash      string
	FlashError bool
}

func (s *Server) getWatcherAddressRetireForm(w http.ResponseWriter, r *http.Request) {
	orderID, err := strconv.ParseInt(chi.URLParam(r, "order_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid order id", http.StatusBadRequest)
		return
	}
	data := watcherRetirePageData{basePageData: s.newBasePageData(r), WatcherTab: "addresses", OrderID: orderID}
	if addr, err := s.Watcher.GetAddress(r.Context(), orderID); err == nil {
		data.Address = watcherAddressRow{Address: addr.Address, Status: addr.Status}
	}
	s.Templates.Render(w, "watcher_retire", data)
}

// postWatcherAddressRetire calls C2's own real POST
// /v1/addresses/{order_id}/retire. confirm_id must equal the order id
// exactly, typed by the operator, before this handler calls C2 at all --
// see opclient.RetireAddress's own doc comment for why this is
// permanent and irreversible.
func (s *Server) postWatcherAddressRetire(w http.ResponseWriter, r *http.Request) {
	orderIDParam := chi.URLParam(r, "order_id")
	orderID, err := strconv.ParseInt(orderIDParam, 10, 64)
	if err != nil {
		http.Error(w, "invalid order id", http.StatusBadRequest)
		return
	}
	render := func(flash string, flashErr bool) {
		data := watcherRetirePageData{basePageData: s.newBasePageData(r), WatcherTab: "addresses", OrderID: orderID, Flash: flash, FlashError: flashErr}
		if addr, err := s.Watcher.GetAddress(r.Context(), orderID); err == nil {
			data.Address = watcherAddressRow{Address: addr.Address, Status: addr.Status}
		}
		s.Templates.Render(w, "watcher_retire", data)
	}
	if err := r.ParseForm(); err != nil {
		render("malformed form submission", true)
		return
	}
	reason := r.FormValue("reason")
	if reason == "" {
		render("reason is required", true)
		return
	}
	if r.FormValue("confirm_id") != orderIDParam {
		render("confirmation did not match order id "+orderIDParam+" -- address was not retired", true)
		return
	}

	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "watcher.address.retire", "order:"+orderIDParam, map[string]any{"reason": reason})

	if _, err := s.Watcher.RetireAddress(r.Context(), orderID, reason); err != nil {
		render("retiring address: "+err.Error(), true)
		return
	}
	http.Redirect(w, r, "/watcher/addresses", http.StatusFound)
}

type orphanedDepositRow struct {
	ID, OrderID                       int64
	ExternalID, TxHash, Amount        string
	DetectedAt, OrderStateAtDetection string
	Resolution                        string
}

type watcherOrphanedPageData struct {
	basePageData
	WatcherTab string
	Deposits   []orphanedDepositRow
	Flash      string
	FlashError bool
	Error      string
}

func (s *Server) renderWatcherOrphaned(w http.ResponseWriter, r *http.Request, flash string, flashErr bool) {
	data := watcherOrphanedPageData{basePageData: s.newBasePageData(r), WatcherTab: "orphaned", Flash: flash, FlashError: flashErr}
	deposits, err := s.Watcher.ListOrphanedDeposits(r.Context(), nil)
	if err != nil {
		data.Error = err.Error()
		s.Templates.Render(w, "watcher_orphaned", data)
		return
	}
	for _, d := range deposits {
		row := orphanedDepositRow{
			ID: d.ID, OrderID: d.OrderID, ExternalID: d.ExternalID, TxHash: d.TxHash, Amount: d.Amount,
			DetectedAt: d.DetectedAt.String(), OrderStateAtDetection: d.OrderStateAtDetection,
		}
		if d.Resolution != nil {
			row.Resolution = *d.Resolution
		}
		data.Deposits = append(data.Deposits, row)
	}
	s.Templates.Render(w, "watcher_orphaned", data)
}

func (s *Server) getWatcherOrphanedDeposits(w http.ResponseWriter, r *http.Request) {
	s.renderWatcherOrphaned(w, r, "", false)
}

func (s *Server) postWatcherOrphanedDepositResolve(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid deposit id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderWatcherOrphaned(w, r, "malformed form submission", true)
		return
	}
	resolution := strings.TrimSpace(r.FormValue("resolution"))
	if resolution == "" {
		s.renderWatcherOrphaned(w, r, "resolution is required", true)
		return
	}

	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "watcher.orphaned_deposit.resolve", "orphaned-deposit:"+strconv.FormatInt(id, 10), map[string]any{"resolution": resolution})

	if _, err := s.Watcher.ResolveOrphanedDeposit(r.Context(), id, resolution); err != nil {
		s.renderWatcherOrphaned(w, r, "resolving: "+err.Error(), true)
		return
	}
	s.renderWatcherOrphaned(w, r, "Orphaned deposit resolved.", false)
}

type watcherProvidersPageData struct {
	basePageData
	WatcherTab string
	Providers  []opclient.ProviderHealth
	Error      string
}

func (s *Server) getWatcherProviders(w http.ResponseWriter, r *http.Request) {
	data := watcherProvidersPageData{basePageData: s.newBasePageData(r), WatcherTab: "providers"}
	providers, err := s.Watcher.GetProviders(r.Context())
	if err != nil {
		data.Error = err.Error()
		s.Templates.Render(w, "watcher_providers", data)
		return
	}
	data.Providers = providers
	s.Templates.Render(w, "watcher_providers", data)
}
