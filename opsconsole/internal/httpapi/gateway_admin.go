package httpapi

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"opsconsole/internal/opclient"
)

// This file is OC.19's own opsconsole half: gateway had no operator
// surface at all before this chunk (docs/03-build/
// admin-panel-build-prompts.md's own "Read this fourth" -- the largest
// real gap that review found), so gateway/internal/httpapi/admin_*.go
// built the five real routes first; these pages are thin wraps of
// those, same confirm/audit conventions as every other write chunk in
// this document.

const gatewayTabs = `
<div class="tabs">
  <a class="tab {{ if eq .GatewayTab "api-keys" }}active{{ end }}" href="/gateway/api-keys">API keys</a>
  <a class="tab {{ if eq .GatewayTab "webhooks" }}active{{ end }}" href="/gateway/webhooks">Webhooks</a>
  <a class="tab {{ if eq .GatewayTab "orders" }}active{{ end }}" href="/gateway/orders">Orders</a>
  <a class="tab {{ if eq .GatewayTab "rate-limits" }}active{{ end }}" href="/gateway/rate-limits">Rate limits</a>
  <a class="tab" href="/sandbox/orders">Sandbox</a>
</div>
`

const gatewayNotConfiguredFlash = `<div class="flash flash-error">` + iconAlert + `<span>Gateway admin isn't configured on this console (OC_GATEWAY_BASE_URL / OC_GATEWAY_ADMIN_TOKEN) -- set both to see this.</span></div>`

const gatewayAPIKeysContent = `
<div class="page-head"><h1>Gateway API keys</h1></div>
` + gatewayTabs + `
<p class="helptext">One key per customer row -- "issue" here rotates that customer's own key (the old one stops authenticating immediately); it does not create a new customer or add a second key alongside an existing one.</p>
{{ if .NotConfigured }}` + gatewayNotConfiguredFlash + `
{{ else }}
{{ if .Flash }}<div class="flash {{ if .FlashError }}flash-error{{ else }}flash-ok{{ end }}">{{ if .FlashError }}` + iconAlert + `{{ else }}` + iconCheck + `{{ end }}<span>{{ .Flash }}</span></div>{{ end }}
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
{{ if .IssuedKey }}
<div class="flash flash-error" style="align-items:flex-start">` + iconAlert + `<span><strong>New key for customer {{ .IssuedKey.ID }}, shown once, right now, never again:</strong><br><span class="mono" style="user-select:all">{{ .IssuedKey.APIKey }}</span><br>Copy it now -- reloading this page will not show it again, only its last 4 characters.</span></div>
{{ end }}
<div class="table-wrap">
<table>
<tr><th>ID</th><th>Name</th><th>Mode</th><th>Status</th><th>Key</th><th>Updated</th><th></th></tr>
{{ range .Keys }}
<tr>
  <td class="mono">{{ .ID }}</td>
  <td>{{ .Name }}</td>
  <td><span class="badge badge-neutral">{{ if .IsSandbox }}sandbox{{ else }}production{{ end }}</span></td>
  <td><span class="badge {{ if eq .Status "active" }}badge-success{{ else }}badge-danger{{ end }}">{{ .Status }}</span></td>
  <td class="mono">{{ if .APIKeyLast4 }}...{{ .APIKeyLast4 }}{{ else }}—{{ end }}</td>
  <td class="mono">{{ .UpdatedAt }}</td>
  <td>
    <div class="actions">
      <form class="inline" method="post" action="/gateway/api-keys/{{ .ID }}/rotate" onsubmit="return confirm('Rotate the key for customer {{ .ID }} ({{ .Name }})? The current key stops working immediately.');">
        <button class="btn btn-sm" type="submit">Rotate</button>
      </form>
      {{ if eq .Status "active" }}
      <form class="inline" method="post" action="/gateway/api-keys/{{ .ID }}/revoke" onsubmit="return confirm('Revoke (suspend) customer {{ .ID }} ({{ .Name }})? Note: for a SANDBOX customer this does not currently block their sandbox routes -- only two production routes (POST /quotes, POST /orders) check active status today.');">
        <button class="btn btn-sm btn-danger" type="submit">Revoke</button>
      </form>
      {{ end }}
    </div>
  </td>
</tr>
{{ else }}
<tr><td colspan="7"><div class="empty-state">No customers yet.</div></td></tr>
{{ end }}
</table>
</div>
{{ end }}
`

const gatewayWebhooksContent = `
<div class="page-head"><h1>Webhook deliveries</h1></div>
` + gatewayTabs + `
<div class="tabs">
  <a class="tab {{ if eq .Status "failed" }}active{{ end }}" href="/gateway/webhooks?status=failed">Failed</a>
  <a class="tab {{ if eq .Status "pending" }}active{{ end }}" href="/gateway/webhooks?status=pending">Pending</a>
  <a class="tab {{ if eq .Status "delivered" }}active{{ end }}" href="/gateway/webhooks?status=delivered">Delivered</a>
  <a class="tab {{ if eq .Status "" }}active{{ end }}" href="/gateway/webhooks">All</a>
</div>
{{ if .NotConfigured }}` + gatewayNotConfiguredFlash + `
{{ else }}
{{ if .Flash }}<div class="flash {{ if .FlashError }}flash-error{{ else }}flash-ok{{ end }}">{{ if .FlashError }}` + iconAlert + `{{ else }}` + iconCheck + `{{ end }}<span>{{ .Flash }}</span></div>{{ end }}
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
<div class="table-wrap">
<table>
<tr><th>ID</th><th>Customer</th><th>Order</th><th>Event</th><th>Attempts</th><th>Next attempt</th><th>Last error</th><th></th></tr>
{{ range .Deliveries }}
<tr>
  <td class="mono">{{ .ID }}</td>
  <td class="mono">{{ .CustomerID }}</td>
  <td class="mono"><a href="/orders/{{ .ExternalID }}">{{ .ExternalID }}</a></td>
  <td>{{ .EventType }}</td>
  <td class="mono">{{ .AttemptCount }}</td>
  <td>{{ if .DeliveredAt }}<span class="badge badge-success">delivered {{ .DeliveredAt }}</span>{{ else }}<span class="mono">{{ .NextAttemptAt }}</span>{{ end }}</td>
  <td style="max-width:220px;overflow:hidden;text-overflow:ellipsis">{{ .LastError }}</td>
  <td>{{ if not .DeliveredAt }}
    <form class="inline" method="post" action="/gateway/webhooks/{{ .ID }}/redrive?status={{ $.Status }}">
      <button class="btn btn-sm" type="submit">Redrive now</button>
    </form>
  {{ end }}</td>
</tr>
{{ else }}
<tr><td colspan="8"><div class="empty-state">No {{ if .Status }}{{ .Status }} {{ end }}deliveries.</div></td></tr>
{{ end }}
</table>
</div>
{{ end }}
`

const gatewayOrdersContent = `
<div class="page-head"><h1>Gateway orders (cross-customer)</h1></div>
` + gatewayTabs + `
<p class="helptext">gateway_orders across every customer_id -- the customer-facing GET /v1/orders/{external_id} stays scoped to the calling API key by design; this is the deliberately separate admin lens on "which customer does this order belong to." Full order state lives on <a href="/orders">Orders</a>, not repeated here.</p>
{{ if .NotConfigured }}` + gatewayNotConfiguredFlash + `
{{ else }}
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
<div class="table-wrap">
<table>
<tr><th>Order</th><th>Customer</th><th>C1 order</th><th>C1 created</th><th>Address assigned</th><th>Created</th></tr>
{{ range .Orders }}
<tr>
  <td class="mono"><a href="/orders/{{ .ExternalID }}">{{ .ExternalID }}</a></td>
  <td class="mono">{{ .CustomerID }}</td>
  <td class="mono">{{ .C1OrderID }}</td>
  <td>{{ if .C1OrderCreated }}` + iconCheck + `{{ end }}</td>
  <td>{{ if .C2AddressAssigned }}` + iconCheck + `{{ end }}</td>
  <td class="mono">{{ .CreatedAt }}</td>
</tr>
{{ else }}
<tr><td colspan="6"><div class="empty-state">No gateway orders yet.</div></td></tr>
{{ end }}
</table>
</div>
{{ end }}
`

const gatewayRateLimitsContent = `
<div class="page-head"><h1>Rate limits</h1></div>
` + gatewayTabs + `
{{ if .NotConfigured }}` + gatewayNotConfiguredFlash + `
{{ else }}
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
{{ if .Note }}<p class="field-hint">{{ .Note }}</p>{{ end }}
<div class="table-wrap">
<table>
<tr><th>Customer</th><th>Tokens remaining</th><th>Per minute</th></tr>
{{ range .Buckets }}
<tr><td class="mono">{{ .CustomerID }}</td><td class="mono">{{ printf "%.1f" .TokensRemaining }}</td><td class="mono">{{ .PerMinute }}</td></tr>
{{ else }}
<tr><td colspan="3"><div class="empty-state">No customer has called this gateway instance since it last started.</div></td></tr>
{{ end }}
</table>
</div>
{{ end }}
`

type gatewayAPIKeyRow struct {
	ID           int64
	Name, Status string
	IsSandbox    bool
	APIKeyLast4  string
	UpdatedAt    string
}

type gatewayAPIKeysPageData struct {
	basePageData
	GatewayTab    string
	NotConfigured bool
	Keys          []gatewayAPIKeyRow
	IssuedKey     *gatewayIssuedKeyView
	Flash         string
	FlashError    bool
	Error         string
}

type gatewayIssuedKeyView struct {
	ID     int64
	APIKey string
}

func (s *Server) renderGatewayAPIKeys(w http.ResponseWriter, r *http.Request, flash string, flashErr bool, issued *gatewayIssuedKeyView) {
	data := gatewayAPIKeysPageData{basePageData: s.newBasePageData(r), GatewayTab: "api-keys", Flash: flash, FlashError: flashErr, IssuedKey: issued}
	if s.Gateway == nil {
		data.NotConfigured = true
		s.Templates.Render(w, "gateway_api_keys", data)
		return
	}
	keys, err := s.Gateway.ListAPIKeys(r.Context(), 0)
	if err != nil {
		data.Error = err.Error()
		s.Templates.Render(w, "gateway_api_keys", data)
		return
	}
	for _, k := range keys {
		row := gatewayAPIKeyRow{ID: k.ID, Name: k.Name, Status: k.Status, IsSandbox: k.IsSandbox, UpdatedAt: k.UpdatedAt.String()}
		if k.APIKeyLast4 != nil {
			row.APIKeyLast4 = *k.APIKeyLast4
		}
		data.Keys = append(data.Keys, row)
	}
	s.Templates.Render(w, "gateway_api_keys", data)
}

func (s *Server) getGatewayAPIKeys(w http.ResponseWriter, r *http.Request) {
	s.renderGatewayAPIKeys(w, r, "", false, nil)
}

// postGatewayAPIKeyRotate calls C6's own real POST /v1/admin/api-keys,
// mode taken from the row's own current is_sandbox (GET
// /v1/admin/api-keys?customer_id=id) rather than guessed by this form
// -- C6 itself would reject a mismatch anyway (mode_mismatch, 409), but
// reading the real current mode first means this button never
// deliberately submits one.
func (s *Server) postGatewayAPIKeyRotate(w http.ResponseWriter, r *http.Request) {
	if s.Gateway == nil {
		http.Error(w, "gateway admin is not configured", http.StatusServiceUnavailable)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid customer id", http.StatusBadRequest)
		return
	}
	existing, err := s.Gateway.ListAPIKeys(r.Context(), id)
	if err != nil || len(existing) == 0 {
		s.renderGatewayAPIKeys(w, r, "looking up customer "+strconv.FormatInt(id, 10)+" before rotating: "+errString(err), true, nil)
		return
	}
	mode := "live"
	if existing[0].IsSandbox {
		mode = "test"
	}

	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "gateway.api_key.rotate", "customer:"+strconv.FormatInt(id, 10), map[string]any{"mode": mode})

	issued, err := s.Gateway.IssueAPIKey(r.Context(), id, mode)
	if err != nil {
		s.renderGatewayAPIKeys(w, r, "rotating key for customer "+strconv.FormatInt(id, 10)+": "+err.Error(), true, nil)
		return
	}
	s.renderGatewayAPIKeys(w, r, "", false, &gatewayIssuedKeyView{ID: issued.ID, APIKey: issued.APIKey})
}

func (s *Server) postGatewayAPIKeyRevoke(w http.ResponseWriter, r *http.Request) {
	if s.Gateway == nil {
		http.Error(w, "gateway admin is not configured", http.StatusServiceUnavailable)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid customer id", http.StatusBadRequest)
		return
	}

	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "gateway.api_key.revoke", "customer:"+strconv.FormatInt(id, 10), nil)

	if _, err := s.Gateway.RevokeAPIKey(r.Context(), id); err != nil {
		s.renderGatewayAPIKeys(w, r, "revoking customer "+strconv.FormatInt(id, 10)+": "+err.Error(), true, nil)
		return
	}
	s.renderGatewayAPIKeys(w, r, "Customer "+strconv.FormatInt(id, 10)+" suspended.", false, nil)
}

type gatewayWebhookRow struct {
	ID, CustomerID             int64
	ExternalID, EventType      string
	AttemptCount               int
	DeliveredAt, NextAttemptAt string
	LastError                  string
}

type gatewayWebhooksPageData struct {
	basePageData
	GatewayTab    string
	NotConfigured bool
	Status        string
	Deliveries    []gatewayWebhookRow
	Flash         string
	FlashError    bool
	Error         string
}

func (s *Server) renderGatewayWebhooks(w http.ResponseWriter, r *http.Request, status, flash string, flashErr bool) {
	data := gatewayWebhooksPageData{basePageData: s.newBasePageData(r), GatewayTab: "webhooks", Status: status, Flash: flash, FlashError: flashErr}
	if s.Gateway == nil {
		data.NotConfigured = true
		s.Templates.Render(w, "gateway_webhooks", data)
		return
	}
	list, err := s.Gateway.ListWebhookDeliveries(r.Context(), status)
	if err != nil {
		data.Error = err.Error()
		s.Templates.Render(w, "gateway_webhooks", data)
		return
	}
	for _, d := range list {
		row := gatewayWebhookRow{
			ID: d.ID, CustomerID: d.CustomerID, ExternalID: d.ExternalID, EventType: d.EventType,
			AttemptCount: d.AttemptCount, NextAttemptAt: d.NextAttemptAt.String(),
		}
		if d.DeliveredAt != nil {
			row.DeliveredAt = d.DeliveredAt.String()
		}
		if d.LastError != nil {
			row.LastError = *d.LastError
		}
		data.Deliveries = append(data.Deliveries, row)
	}
	s.Templates.Render(w, "gateway_webhooks", data)
}

func (s *Server) getGatewayWebhooks(w http.ResponseWriter, r *http.Request) {
	s.renderGatewayWebhooks(w, r, r.URL.Query().Get("status"), "", false)
}

func (s *Server) postGatewayWebhookRedrive(w http.ResponseWriter, r *http.Request) {
	if s.Gateway == nil {
		http.Error(w, "gateway admin is not configured", http.StatusServiceUnavailable)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid delivery id", http.StatusBadRequest)
		return
	}
	status := r.URL.Query().Get("status")

	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "gateway.webhook.redrive", "delivery:"+strconv.FormatInt(id, 10), nil)

	d, err := s.Gateway.RedriveWebhookDelivery(r.Context(), id)
	if err != nil {
		s.renderGatewayWebhooks(w, r, status, "redriving delivery "+strconv.FormatInt(id, 10)+": "+err.Error(), true)
		return
	}
	s.renderGatewayWebhooks(w, r, status, "Delivery "+strconv.FormatInt(id, 10)+" redriven -- attempt "+strconv.Itoa(d.AttemptCount)+".", false)
}

type gatewayOrderRow struct {
	ExternalID                        string
	CustomerID, C1OrderID             int64
	C1OrderCreated, C2AddressAssigned bool
	CreatedAt                         string
}

type gatewayOrdersPageData struct {
	basePageData
	GatewayTab    string
	NotConfigured bool
	Orders        []gatewayOrderRow
	Error         string
}

func (s *Server) getGatewayOrders(w http.ResponseWriter, r *http.Request) {
	data := gatewayOrdersPageData{basePageData: s.newBasePageData(r), GatewayTab: "orders"}
	if s.Gateway == nil {
		data.NotConfigured = true
		s.Templates.Render(w, "gateway_orders", data)
		return
	}
	list, err := s.Gateway.ListAdminOrders(r.Context())
	if err != nil {
		data.Error = err.Error()
		s.Templates.Render(w, "gateway_orders", data)
		return
	}
	for _, o := range list {
		data.Orders = append(data.Orders, gatewayOrderRow{
			ExternalID: o.ExternalID, CustomerID: o.CustomerID, C1OrderID: o.C1OrderID,
			C1OrderCreated: o.C1OrderCreated, C2AddressAssigned: o.C2AddressAssigned, CreatedAt: o.CreatedAt.String(),
		})
	}
	s.Templates.Render(w, "gateway_orders", data)
}

type gatewayRateLimitsPageData struct {
	basePageData
	GatewayTab    string
	NotConfigured bool
	Buckets       []opclient.RateLimitBucket
	Note          string
	Error         string
}

func (s *Server) getGatewayRateLimits(w http.ResponseWriter, r *http.Request) {
	data := gatewayRateLimitsPageData{basePageData: s.newBasePageData(r), GatewayTab: "rate-limits"}
	if s.Gateway == nil {
		data.NotConfigured = true
		s.Templates.Render(w, "gateway_rate_limits", data)
		return
	}
	snapshot, err := s.Gateway.GetRateLimits(r.Context())
	if err != nil {
		data.Error = err.Error()
		s.Templates.Render(w, "gateway_rate_limits", data)
		return
	}
	data.Buckets = snapshot.Buckets
	data.Note = snapshot.Note
	s.Templates.Render(w, "gateway_rate_limits", data)
}

func errString(err error) string {
	if err == nil {
		return "no key found for that customer"
	}
	return err.Error()
}
