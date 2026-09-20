package httpapi

import (
	"net/http"
)

const sandboxOrdersContent = `
<div class="page-head"><h1>Sandbox orders</h1></div>
<p class="helptext">
  Real orders in C6's own isolated sandbox system -- deterministic,
  scripted outcomes, zero real money, never mixed with production data
  (a sandbox external_id always starts with sbx_). Useful for showing
  the pipeline without waiting on or spending anything real.
</p>
{{ if .NotConfigured }}
<div class="flash flash-error">` + iconAlert + `<span>Gateway isn't configured on this console (OC_GATEWAY_BASE_URL / OC_GATEWAY_SANDBOX_KEY) -- set both to see sandbox orders here.</span></div>
{{ else if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
<div class="table-wrap">
<table>
<tr><th>External ID</th><th>Trigger</th><th>Tier</th><th>Amount in</th><th>Amount out</th><th>State</th><th>Webhook</th></tr>
{{ range .Orders }}
<tr>
  <td class="mono">{{ .ExternalID }}</td>
  <td><span class="badge badge-neutral">{{ .Trigger }}</span></td>
  <td>{{ .Tier }}</td>
  <td>{{ .AmountIn }}</td>
  <td>{{ .AmountOut }}</td>
  <td>
    <span class="badge {{ if eq .State "settled" }}badge-success{{ else if eq .State "held" }}badge-danger{{ else }}badge-warning{{ end }}">{{ .State }}</span>
    {{ if .HoldReason }}<div class="field-hint" style="margin:2px 0 0">{{ .HoldReason }}</div>{{ end }}
  </td>
  <td>{{ if .WebhookExhausted }}<span class="badge badge-danger">exhausted after {{ .WebhookAttempts }}</span>{{ else if .WebhookAttempts }}{{ .WebhookAttempts }} attempts{{ else }}—{{ end }}</td>
</tr>
{{ else }}
<tr><td colspan="7"><div class="empty-state">No sandbox orders yet.</div></td></tr>
{{ end }}
</table>
</div>
`

type sandboxOrderRow struct {
	ExternalID       string
	Trigger          string
	Tier             string
	AmountIn         string
	AmountOut        string
	State            string
	HoldReason       string
	WebhookAttempts  int
	WebhookExhausted bool
}

type sandboxOrdersPageData struct {
	basePageData
	Orders        []sandboxOrderRow
	Error         string
	NotConfigured bool
}

func (s *Server) getSandboxOrders(w http.ResponseWriter, r *http.Request) {
	data := sandboxOrdersPageData{basePageData: s.newBasePageData(r)}
	if s.Gateway == nil {
		data.NotConfigured = true
		s.Templates.Render(w, "sandbox_orders", data)
		return
	}
	orders, err := s.Gateway.ListSandboxOrders(r.Context())
	if err != nil {
		data.Error = err.Error()
	}
	for _, o := range orders {
		row := sandboxOrderRow{
			ExternalID: o.ExternalID, Trigger: o.Trigger, Tier: o.Tier, AmountIn: o.AmountIn, AmountOut: o.AmountOut,
			State: o.State, WebhookAttempts: o.WebhookAttempts, WebhookExhausted: o.WebhookExhausted,
		}
		if o.HoldReason != nil {
			row.HoldReason = *o.HoldReason
		}
		data.Orders = append(data.Orders, row)
	}
	s.Templates.Render(w, "sandbox_orders", data)
}
