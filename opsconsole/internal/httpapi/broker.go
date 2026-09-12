package httpapi

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"opsconsole/internal/opclient"
)

const brokerReservationsContent = `
<h1>Broker reservations</h1>
<p>
  <a href="/broker/reservations?status=FAILED">FAILED</a> ·
  <a href="/broker/reservations?status=PENDING">PENDING</a> ·
  <a href="/broker/reservations?status=CONFIRMED">CONFIRMED</a> ·
  <a href="/broker/fallback-events">fallback events</a>
</p>
{{ if .Error }}<div class="flash flash-error">{{ .Error }}</div>{{ end }}
<table>
<tr><th>ID</th><th>External ID</th><th>Order</th><th>Target</th><th>Units</th><th>Status</th><th>Vendor</th><th>Deadline</th><th></th></tr>
{{ range .Reservations }}
<tr>
  <td>{{ .ID }}</td><td>{{ .ExternalID }}</td><td>{{ .OrderID }}</td><td>{{ .TargetAddress }}</td>
  <td>{{ .EnergyUnits }}</td><td>{{ .Status }}</td><td>{{ if .Vendor }}{{ .Vendor }}{{ end }}</td><td>{{ .Deadline }}</td>
  <td>{{ if eq .Status "FAILED" }}<a href="/broker/reservations/{{ .ID }}/reconcile">reconcile</a>{{ end }}</td>
</tr>
{{ end }}
</table>
`

const brokerReconcileContent = `
<h1>Reconcile reservation {{ .ID }}</h1>
{{ if .Error }}<div class="flash flash-error">{{ .Error }}</div>{{ end }}
<p>The operator is vouching for a delegation independently verified real -- e.g. read directly on-chain against the vendor's own target address. This is not automatic.</p>
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
  <p><button class="danger" type="submit">Confirm reservation</button></p>
</form>
`

const brokerFallbackContent = `
<h1>Manual fallback events</h1>
{{ if .Error }}<div class="flash flash-error">{{ .Error }}</div>{{ end }}
<table>
<tr><th>ID</th><th>Reason</th><th>Triggered</th><th>Resolved</th><th></th></tr>
{{ range .Events }}
<tr>
  <td>{{ .ID }}</td><td>{{ .Reason }}</td><td>{{ .TriggeredAt }}</td>
  <td>{{ if .ResolvedAt }}{{ .ResolvedAt }}{{ else }}open{{ end }}</td>
  <td>{{ if not .ResolvedAt }}
    <form class="inline" method="post" action="/broker/fallback-events/{{ .ID }}/resolve">
      <input type="text" name="resolution" placeholder="resolution" required>
      <button type="submit">Resolve</button>
    </form>
  {{ end }}</td>
</tr>
{{ end }}
</table>
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
	Reservations []reservationRow
	Error        string
}

func (s *Server) getBrokerReservations(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "FAILED"
	}
	data := brokerReservationsPageData{basePageData: s.newBasePageData(r)}
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
