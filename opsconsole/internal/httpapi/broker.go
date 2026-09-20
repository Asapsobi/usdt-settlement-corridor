package httpapi

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"opsconsole/internal/opclient"
)

const brokerReservationsContent = `
<div class="page-head"><h1>Broker reservations</h1></div>
<div class="tabs">
  <a class="tab {{ if eq .Status "FAILED" }}active{{ end }}" href="/broker/reservations?status=FAILED">Failed</a>
  <a class="tab {{ if eq .Status "PENDING" }}active{{ end }}" href="/broker/reservations?status=PENDING">Pending</a>
  <a class="tab {{ if eq .Status "CONFIRMED" }}active{{ end }}" href="/broker/reservations?status=CONFIRMED">Confirmed</a>
  <a class="tab" href="/broker/fallback-events">Fallback events</a>
  <a class="tab" href="/broker/providers">Providers</a>
</div>
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
<div class="table-wrap">
<table>
<tr><th>ID</th><th>External ID</th><th>Order</th><th>Target</th><th>Units</th><th>Status</th><th>Vendor</th><th>Deadline</th><th></th></tr>
{{ range .Reservations }}
<tr>
  <td class="mono">{{ .ID }}</td><td>{{ .ExternalID }}</td><td>{{ .OrderID }}</td><td class="mono">{{ .TargetAddress }}</td>
  <td>{{ .EnergyUnits }}</td>
  <td><span class="badge {{ if eq .Status "CONFIRMED" }}badge-success{{ else if eq .Status "FAILED" }}badge-danger{{ else }}badge-warning{{ end }}">{{ .Status }}</span></td>
  <td>{{ if .Vendor }}{{ .Vendor }}{{ end }}</td><td class="mono">{{ .Deadline }}</td>
  <td>{{ if eq .Status "FAILED" }}<a class="btn btn-sm" href="/broker/reservations/{{ .ID }}/reconcile">Reconcile</a>{{ end }}</td>
</tr>
{{ else }}
<tr><td colspan="9"><div class="empty-state">No {{ .Status }} reservations right now.</div></td></tr>
{{ end }}
</table>
</div>
`

const brokerReconcileContent = `
<div class="page-head"><h1>Reconcile reservation {{ .ID }}</h1></div>
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
<p class="helptext">The operator is vouching for a delegation independently verified real -- e.g. read directly on-chain against the vendor's own target address. This is not automatic.</p>
<div class="card panel-narrow">
<form method="post" action="/broker/reservations/{{ .ID }}/reconcile">
  <label>order_id</label>
  <input type="number" name="order_id" required>
  <label>provider</label>
  <input type="text" name="provider" required>
  <label>delegation_id</label>
  <input type="text" name="delegation_id" required>
  <label>target_address</label>
  <input type="text" name="target_address" required>
  <label>energy_units</label>
  <input type="number" name="energy_units" required>
  <label>cost_trx (decimal TRX, e.g. 1.950000)</label>
  <input type="text" name="cost_trx" required>
  <p style="margin-top:16px"><button class="btn btn-danger" type="submit">Confirm reservation</button></p>
</form>
</div>
`

const brokerFallbackContent = `
<div class="page-head"><h1>Manual fallback events</h1></div>
<div class="tabs">
  <a class="tab" href="/broker/reservations?status=FAILED">Failed</a>
  <a class="tab" href="/broker/reservations?status=PENDING">Pending</a>
  <a class="tab" href="/broker/reservations?status=CONFIRMED">Confirmed</a>
  <a class="tab active" href="/broker/fallback-events">Fallback events</a>
  <a class="tab" href="/broker/providers">Providers</a>
</div>
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
<div class="table-wrap">
<table>
<tr><th>ID</th><th>Reason</th><th>Triggered</th><th>Resolved</th><th></th></tr>
{{ range .Events }}
<tr>
  <td class="mono">{{ .ID }}</td><td>{{ .Reason }}</td><td class="mono">{{ .TriggeredAt }}</td>
  <td>{{ if .ResolvedAt }}<span class="mono">{{ .ResolvedAt }}</span>{{ else }}<span class="badge badge-warning">open</span>{{ end }}</td>
  <td>{{ if not .ResolvedAt }}
    <form class="inline" method="post" action="/broker/fallback-events/{{ .ID }}/resolve">
      <div class="actions">
        <input type="text" name="resolution" placeholder="resolution" required style="width:180px">
        <button class="btn btn-sm" type="submit">Resolve</button>
      </div>
    </form>
  {{ end }}</td>
</tr>
{{ else }}
<tr><td colspan="5"><div class="empty-state">No fallback events.</div></td></tr>
{{ end }}
</table>
</div>
`

type reservationRow struct {
	ID, OrderID               int64
	ExternalID, TargetAddress string
	EnergyUnits               int64
	Status                    string
	Vendor                    string
	Deadline                  string
}

type brokerReservationsPageData struct {
	basePageData
	Status       string
	Reservations []reservationRow
	Error        string
}

func (s *Server) getBrokerReservations(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "FAILED"
	}
	data := brokerReservationsPageData{basePageData: s.newBasePageData(r), Status: status}
	rows, err := s.Broker.ListReservations(r.Context(), []string{status}, 100)
	if err != nil {
		data.Error = err.Error()
	}
	for _, res := range rows {
		row := reservationRow{
			ID: res.ID, OrderID: res.OrderID, ExternalID: res.ExternalID, TargetAddress: res.TargetAddress,
			EnergyUnits: res.EnergyUnits, Status: res.Status, Deadline: res.Deadline.String(),
		}
		if res.Vendor != nil {
			row.Vendor = *res.Vendor
		}
		data.Reservations = append(data.Reservations, row)
	}
	s.Templates.Render(w, "broker_reservations", data)
}

type brokerReconcilePageData struct {
	basePageData
	ID    int64
	Error string
}

func (s *Server) getBrokerReconcileForm(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid reservation id", http.StatusBadRequest)
		return
	}
	s.Templates.Render(w, "broker_reconcile", brokerReconcilePageData{basePageData: s.newBasePageData(r), ID: id})
}

func (s *Server) postBrokerReconcile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid reservation id", http.StatusBadRequest)
		return
	}
	render := func(errMsg string) {
		s.Templates.Render(w, "broker_reconcile", brokerReconcilePageData{basePageData: s.newBasePageData(r), ID: id, Error: errMsg})
	}
	if err := r.ParseForm(); err != nil {
		render("malformed form submission")
		return
	}
	orderID, err := strconv.ParseInt(r.FormValue("order_id"), 10, 64)
	if err != nil {
		render("order_id must be an integer")
		return
	}
	energyUnits, err := strconv.ParseInt(r.FormValue("energy_units"), 10, 64)
	if err != nil {
		render("energy_units must be an integer")
		return
	}
	req := opclient.ReconcileRequest{
		OrderID: orderID, Provider: r.FormValue("provider"), DelegationID: r.FormValue("delegation_id"),
		TargetAddress: r.FormValue("target_address"), EnergyUnits: energyUnits, CostTRX: r.FormValue("cost_trx"),
	}

	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "broker.reservation.reconcile", "reservation:"+strconv.FormatInt(id, 10), map[string]any{"request": req})
	if _, err := s.Broker.ReconcileReservation(r.Context(), id, req); err != nil {
		render("reconciling: " + err.Error())
		return
	}
	http.Redirect(w, r, "/broker/reservations?status=CONFIRMED", http.StatusFound)
}

type fallbackEventRow struct {
	ID          int64
	Reason      string
	TriggeredAt string
	ResolvedAt  string
}

type brokerFallbackPageData struct {
	basePageData
	Events []fallbackEventRow
	Error  string
}

func (s *Server) getBrokerFallbackEvents(w http.ResponseWriter, r *http.Request) {
	data := brokerFallbackPageData{basePageData: s.newBasePageData(r)}
	events, err := s.Broker.ListFallbackEvents(r.Context(), nil)
	if err != nil {
		data.Error = err.Error()
	}
	for _, e := range events {
		row := fallbackEventRow{ID: e.ID, Reason: e.Reason, TriggeredAt: e.TriggeredAt.String()}
		if e.ResolvedAt != nil {
			row.ResolvedAt = e.ResolvedAt.String()
		}
		data.Events = append(data.Events, row)
	}
	s.Templates.Render(w, "broker_fallback", data)
}

func (s *Server) postBrokerFallbackResolve(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid event id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/broker/fallback-events", http.StatusFound)
		return
	}
	resolution := r.FormValue("resolution")
	sess, _ := sessionFromContext(r.Context())

	_ = s.Audit.Write(sess.Username, "broker.fallback.resolve", "fallback-event:"+strconv.FormatInt(id, 10), map[string]any{"resolution": resolution})
	if err := s.Broker.ResolveFallbackEvent(r.Context(), id, resolution, sess.DisplayName); err != nil {
		data := brokerFallbackPageData{basePageData: s.newBasePageData(r), Error: "resolving: " + err.Error()}
		if events, listErr := s.Broker.ListFallbackEvents(r.Context(), nil); listErr == nil {
			for _, e := range events {
				row := fallbackEventRow{ID: e.ID, Reason: e.Reason, TriggeredAt: e.TriggeredAt.String()}
				if e.ResolvedAt != nil {
					row.ResolvedAt = e.ResolvedAt.String()
				}
				data.Events = append(data.Events, row)
			}
		}
		s.Templates.Render(w, "broker_fallback", data)
		return
	}
	http.Redirect(w, r, "/broker/fallback-events", http.StatusFound)
}

const brokerProvidersContent = `
<div class="page-head"><h1>Energy vendors</h1></div>
<div class="tabs">
  <a class="tab" href="/broker/reservations?status=FAILED">Failed</a>
  <a class="tab" href="/broker/reservations?status=PENDING">Pending</a>
  <a class="tab" href="/broker/reservations?status=CONFIRMED">Confirmed</a>
  <a class="tab" href="/broker/fallback-events">Fallback events</a>
  <a class="tab active" href="/broker/providers">Providers</a>
</div>
{{ if .Flash }}<div class="flash {{ if .FlashError }}flash-error{{ else }}flash-ok{{ end }}">{{ if .FlashError }}` + iconAlert + `{{ else }}` + iconCheck + `{{ end }}<span>{{ .Flash }}</span></div>{{ end }}
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
<p class="helptext">Changes here take effect the next time brokerd restarts -- they don't hot-swap the live, already-running vendor connections.</p>
<div class="table-wrap section">
<table>
<tr><th>Provider</th><th>Base URL</th><th>API Key</th><th>Secret</th><th>Status</th><th>Updated</th></tr>
{{ range .Providers }}
<tr>
  <td style="text-transform:capitalize">{{ .ProviderName }}</td>
  <td class="mono">{{ if .BaseURL }}{{ .BaseURL }}{{ else }}—{{ end }}</td>
  <td class="mono">{{ .APIKeyMasked }}</td>
  <td>{{ if .HasSecret }}<span class="badge badge-neutral">set</span>{{ else }}—{{ end }}</td>
  <td><span class="badge {{ if .Enabled }}badge-success{{ else }}badge-neutral{{ end }}">{{ if .Enabled }}Enabled{{ else }}Disabled{{ end }}</span></td>
  <td class="mono">{{ .UpdatedAt }} by {{ .UpdatedBy }}</td>
</tr>
{{ else }}
<tr><td colspan="6"><div class="empty-state">No vendors configured yet -- add one below.</div></td></tr>
{{ end }}
</table>
</div>

<div class="card panel-narrow">
  <h2 style="margin-bottom:10px">Add / rotate a vendor</h2>
  <form method="post" action="/broker/providers">
    <label>Provider</label>
    <select name="provider_name" required>
      <option value="catfee">CatFee</option>
      <option value="tronsell">Tronsell</option>
      <option value="netts">Netts</option>
    </select>
    <label>API key</label>
    <input type="text" name="api_key" required>
    <label>API secret <span style="font-weight:400">(required for CatFee/Tronsell, unused for Netts)</span></label>
    <input type="password" name="api_secret">
    <label>Base URL <span style="font-weight:400">(required for Tronsell only)</span></label>
    <input type="text" name="base_url" placeholder="https://...">
    <label>Real IP <span style="font-weight:400">(required for Netts only -- your whitelisted egress IP)</span></label>
    <input type="text" name="real_ip" placeholder="203.0.113.1">
    <p><label style="display:inline;margin:0;font-weight:400"><input type="checkbox" name="enabled" value="true" checked style="width:auto;vertical-align:middle"> Enabled</label></p>
    <p style="margin-top:16px"><button class="btn btn-primary" type="submit">Save</button></p>
  </form>
</div>
`

type providerRow struct {
	ProviderName string
	BaseURL      string
	APIKeyMasked string
	HasSecret    bool
	RealIP       string
	Enabled      bool
	UpdatedAt    string
	UpdatedBy    string
}

type brokerProvidersPageData struct {
	basePageData
	Providers  []providerRow
	Flash      string
	FlashError bool
	Error      string
}

func (s *Server) renderBrokerProviders(w http.ResponseWriter, r *http.Request, flash string, flashErr bool) {
	data := brokerProvidersPageData{basePageData: s.newBasePageData(r), Flash: flash, FlashError: flashErr}
	creds, err := s.Broker.ListProviderCredentials(r.Context())
	if err != nil {
		data.Error = err.Error()
	}
	for _, c := range creds {
		row := providerRow{ProviderName: c.ProviderName, APIKeyMasked: c.APIKeyMasked, HasSecret: c.HasSecret, Enabled: c.Enabled, UpdatedAt: c.UpdatedAt, UpdatedBy: c.UpdatedBy}
		if c.BaseURL != nil {
			row.BaseURL = *c.BaseURL
		}
		if c.RealIP != nil {
			row.RealIP = *c.RealIP
		}
		data.Providers = append(data.Providers, row)
	}
	s.Templates.Render(w, "broker_providers", data)
}

func (s *Server) getBrokerProviders(w http.ResponseWriter, r *http.Request) {
	s.renderBrokerProviders(w, r, "", false)
}

func (s *Server) postBrokerProviders(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderBrokerProviders(w, r, "malformed form submission", true)
		return
	}
	sess, _ := sessionFromContext(r.Context())
	req := opclient.UpsertProviderCredentialRequest{
		ProviderName: r.FormValue("provider_name"),
		BaseURL:      r.FormValue("base_url"),
		APIKey:       r.FormValue("api_key"),
		APISecret:    r.FormValue("api_secret"),
		RealIP:       r.FormValue("real_ip"),
		Enabled:      r.FormValue("enabled") == "true",
		UpdatedBy:    sess.DisplayName,
	}

	_ = s.Audit.Write(sess.Username, "broker.provider.upsert", "provider:"+req.ProviderName, map[string]any{
		"base_url_set": req.BaseURL != "", "real_ip_set": req.RealIP != "", "enabled": req.Enabled,
	})
	if _, err := s.Broker.UpsertProviderCredential(r.Context(), req); err != nil {
		s.renderBrokerProviders(w, r, "saving: "+err.Error(), true)
		return
	}
	s.renderBrokerProviders(w, r, req.ProviderName+" saved -- restart brokerd to apply.", false)
}
