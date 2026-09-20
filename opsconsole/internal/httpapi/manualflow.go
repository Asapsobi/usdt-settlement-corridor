package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"opsconsole/internal/opclient"
)

const manualFlowContent = `
<div class="page-head"><h1>Manual flow</h1></div>
<p class="helptext">
  Walks one payout through the real pipeline by hand, entirely from this
  page -- no curl, no background loop. Creating an order here calls the
  same real driver the E2E proof runs use: a genuine BSC deposit address
  is generated, and a genuine deposit to it is what moves this order
  forward through screening and TRC20 dispatch. This is real money end to
  end, not a simulation.
</p>
{{ if .NotConfigured }}
<div class="flash flash-error">` + iconAlert + `<span>Proofrun isn't configured on this console (OC_PROOFRUN_BASE_URL) -- set it to use this page.</span></div>
{{ else }}
{{ if .Flash }}<div class="flash {{ if .FlashError }}flash-error{{ else }}flash-ok{{ end }}">{{ if .FlashError }}` + iconAlert + `{{ else }}` + iconCheck + `{{ end }}<span>{{ .Flash }}</span></div>{{ end }}

<div class="card panel-narrow">
  <h2 style="margin-bottom:10px">1. Create order</h2>
  <p class="field-hint" style="margin:0 0 10px">Generates a real BSC deposit address for this order.</p>
  <form method="post" action="/manual/payout">
    <label>external_id</label>
    <input type="text" name="external_id" value="{{ .SuggestedExternalID }}" required>
    <label>customer_id</label>
    <input type="text" name="customer_id" value="manual-ops-console" required>
    <label>recipient_tron_address (TRC20 payout wallet)</label>
    <input type="text" name="recipient_tron_address" placeholder="T..." required>
    <label>amount_in (USDT, BEP20)</label>
    <input type="text" name="amount_in" placeholder="25.000000" required>
    <p style="margin-top:16px"><button class="btn btn-primary" type="submit">Create order &amp; get deposit address</button></p>
  </form>
</div>

{{ if .Status }}
<div class="card panel-narrow section">
  <h2 style="margin-bottom:10px">2. Order {{ .Status.ExternalID }}</h2>
  <div class="fact"><span class="fact-label">state</span><span class="fact-value"><span class="badge {{ if eq .Status.State "settled" }}badge-success{{ else if eq .Status.State "held" }}badge-danger{{ else if eq .Status.State "refunded" }}badge-neutral{{ else if eq .Status.State "expired" }}badge-neutral{{ else }}badge-warning{{ end }}">{{ .Status.State }}</span></span></div>
  <div class="fact"><span class="fact-label">amount_in / amount_out</span><span class="fact-value">{{ .Status.AmountIn }} / {{ .Status.AmountOut }}</span></div>
  <div class="fact"><span class="fact-label">recipient (TRC20)</span><span class="fact-value mono">{{ .Status.RecipientAddress }}</span></div>
  {{ if .Status.DepositAddress }}
  <div class="fact"><span class="fact-label">deposit address (BSC)</span><span class="fact-value mono">{{ .Status.DepositAddress }}</span></div>
  <div class="fact"><span class="fact-label">deposit address status</span><span class="fact-value"><span class="badge {{ if eq .Status.AddressStatus "FUNDED" }}badge-success{{ else if eq .Status.AddressStatus "RETIRED" }}badge-neutral{{ else }}badge-warning{{ end }}">{{ .Status.AddressStatus }}</span></span></div>
  {{ end }}
  {{ if .Status.AddressError }}<div class="flash flash-error" style="margin-top:8px">` + iconAlert + `<span>{{ .Status.AddressError }}</span></div>{{ end }}
  {{ if .Status.DispatchStatus }}
  <div class="fact"><span class="fact-label">dispatch status</span><span class="fact-value"><span class="badge {{ if eq .Status.DispatchStatus "SETTLED" }}badge-success{{ else if eq .Status.DispatchStatus "HELD" }}badge-danger{{ else }}badge-warning{{ end }}">{{ .Status.DispatchStatus }}</span></span></div>
  {{ end }}
  {{ if .Status.DispatchError }}<div class="flash flash-error" style="margin-top:8px">` + iconAlert + `<span>{{ .Status.DispatchError }}</span></div>{{ end }}
  {{ if .Status.PayoutTxHash }}<div class="fact"><span class="fact-label">payout tx (TRC20)</span><span class="fact-value mono">{{ .Status.PayoutTxHash }}</span></div>{{ end }}
  <p style="margin-top:16px">
    <a class="btn btn-sm" href="/manual/payout?order={{ .Status.ExternalID }}">Refresh status</a>
    {{ if eq .Status.State "held" }}<a class="btn btn-sm" href="/screening/holds">Go to screening holds</a>{{ end }}
  </p>
  <p class="field-hint" style="margin-top:10px">{{ .Status.Note }}</p>
</div>
{{ end }}
{{ end }}
`

// payoutStatusView flattens opclient.PayoutStatus's pointer fields (nil
// until that component of the pipeline is reached) into plain strings
// for the template -- html/template's eq can't compare a *string
// against a string literal, and every other page in this console
// already dereferences pointers before handing data to a template
// (see sandboxOrderRow's HoldReason in gateway.go).
type payoutStatusView struct {
	ExternalID       string
	State            string
	AmountIn         string
	AmountOut        string
	RecipientAddress string
	DepositAddress   string
	AddressStatus    string
	AddressError     string
	DispatchStatus   string
	DispatchError    string
	PayoutTxHash     string
	Note             string
}

type manualFlowPageData struct {
	basePageData
	NotConfigured       bool
	Flash               string
	FlashError          bool
	SuggestedExternalID string
	Status              *payoutStatusView
}

func randExternalID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "manual-" + hex.EncodeToString(b)
}

func (s *Server) renderManualFlow(w http.ResponseWriter, r *http.Request, flash string, flashErr bool) {
	data := manualFlowPageData{
		basePageData: s.newBasePageData(r), SuggestedExternalID: randExternalID(),
		Flash: flash, FlashError: flashErr,
	}
	if s.Proofrun == nil {
		data.NotConfigured = true
		s.Templates.Render(w, "manual_flow", data)
		return
	}
	if order := r.URL.Query().Get("order"); order != "" {
		status, err := s.Proofrun.GetPayout(r.Context(), order)
		if err != nil {
			if data.Flash == "" {
				data.Flash, data.FlashError = "fetching status: "+err.Error(), true
			}
		} else {
			view := payoutStatusView{
				ExternalID: status.ExternalID, State: status.State,
				AmountIn: status.AmountIn, AmountOut: status.AmountOut,
				RecipientAddress: status.RecipientAddress,
				AddressError:     status.AddressError, DispatchError: status.DispatchError,
				Note: status.Note,
			}
			if status.DepositAddress != nil {
				view.DepositAddress = *status.DepositAddress
			}
			if status.AddressStatus != nil {
				view.AddressStatus = *status.AddressStatus
			}
			if status.DispatchStatus != nil {
				view.DispatchStatus = *status.DispatchStatus
			}
			if status.PayoutTxHash != nil {
				view.PayoutTxHash = *status.PayoutTxHash
			}
			data.Status = &view
		}
	}
	s.Templates.Render(w, "manual_flow", data)
}

func (s *Server) getManualFlow(w http.ResponseWriter, r *http.Request) {
	s.renderManualFlow(w, r, "", false)
}

func (s *Server) postManualFlow(w http.ResponseWriter, r *http.Request) {
	if s.Proofrun == nil {
		s.renderManualFlow(w, r, "proofrun isn't configured", true)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderManualFlow(w, r, "malformed form submission", true)
		return
	}
	req := opclient.CreatePayoutRequest{
		ExternalID:           r.FormValue("external_id"),
		CustomerID:           r.FormValue("customer_id"),
		RecipientTronAddress: r.FormValue("recipient_tron_address"),
		AmountIn:             r.FormValue("amount_in"),
	}
	if req.ExternalID == "" || req.CustomerID == "" || req.RecipientTronAddress == "" || req.AmountIn == "" {
		s.renderManualFlow(w, r, "external_id, customer_id, recipient_tron_address, and amount_in are all required", true)
		return
	}

	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "manual_flow.payout.create", "proofrun", map[string]any{
		"external_id": req.ExternalID, "customer_id": req.CustomerID,
		"recipient_tron_address": req.RecipientTronAddress, "amount_in": req.AmountIn,
	})
	result, err := s.Proofrun.CreatePayout(r.Context(), req)
	if err != nil {
		s.renderManualFlow(w, r, "creating payout: "+err.Error(), true)
		return
	}
	http.Redirect(w, r, "/manual/payout?order="+result.ExternalID, http.StatusSeeOther)
}
