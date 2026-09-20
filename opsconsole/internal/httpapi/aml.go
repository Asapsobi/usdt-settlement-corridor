package httpapi

import "net/http"

const amlExposureContent = `
<div class="page-head"><h1>AML exposure</h1></div>
<p class="helptext">
  Every open hold, grouped by the counterparty address that funded it --
  built from C3's own open holds joined against C1's order record for
  each (there is no route to browse "every flagged address" directly;
  GET /v1/screening-results needs an address you already know, so this
  view discovers addresses via holds, the same data C3 itself decided
  to flag). Read-only: releasing or rejecting a hold is
  <a href="/screening/holds">Screening &gt; Holds</a>' job, not duplicated here.
</p>
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
{{ range .Counterparties }}
<div class="card section">
  <div class="card-head">
    <h2 class="mono" style="font-size:14px">{{ .Address }}</h2>
    {{ if .VerdictKnown }}<span class="badge badge-danger" style="margin-left:auto">risk {{ .RiskScore }}</span>{{ end }}
  </div>
  {{ if .VerdictKnown }}
  <div class="fact"><span class="fact-label">provider</span><span class="fact-value">{{ .Provider }}</span></div>
  <div class="fact"><span class="fact-label">reason_codes</span><span class="fact-value">{{ .ReasonCodes }}</span></div>
  <div class="fact"><span class="fact-label">checked_at</span><span class="fact-value mono">{{ .CheckedAt }}</span></div>
  {{ else }}
  <p class="field-hint" style="margin:0 0 10px">C3 has no screening-result history for this address (flagged by a mechanism other than a cached provider check, or the lookup itself failed -- see below).</p>
  {{ end }}
  <div class="table-wrap" style="margin-top:12px">
  <table>
  <tr><th>Order</th><th>State</th><th>Our deposit address</th><th>Hold</th></tr>
  {{ range .Orders }}
  <tr>
    <td class="mono"><a href="/orders/{{ .ExternalID }}">{{ .ExternalID }}</a></td>
    <td><span class="badge badge-danger">{{ .State }}</span></td>
    <td class="mono">{{ if .DepositAddress }}{{ .DepositAddress }}{{ else }}<span style="color:var(--text-muted)">no linked deposit found</span>{{ end }}</td>
    <td>{{ .ReasonCode }}</td>
  </tr>
  {{ end }}
  </table>
  </div>
</div>
{{ else }}
<div class="empty-state">No open holds right now.</div>
{{ end }}
`

type amlLinkedOrder struct {
	ExternalID     string
	State          string
	DepositAddress string
	ReasonCode     string
}

type amlCounterparty struct {
	Address      string
	VerdictKnown bool
	Provider     string
	RiskScore    float64
	ReasonCodes  string
	CheckedAt    string
	Orders       []amlLinkedOrder
}

// getAMLExposure builds admin-panel-build-prompts.md's OC.13 entirely
// from existing reads -- no new backend route, per that chunk's own
// "no new business logic to invent." Discovery starts from C3's own
// open holds (the only real way to find a flagged address at all, since
// GET /v1/screening-results needs one already known), joined against
// each hold's own funding order (C1) for the counterparty's own
// sender_address and current order state, and C2 for which of our own
// deposit addresses that counterparty actually paid. Groups by
// counterparty so one address touching several orders is one row with
// several linked orders, never duplicated.
func (s *Server) getAMLExposure(w http.ResponseWriter, r *http.Request) {
	data := struct {
		basePageData
		Counterparties []amlCounterparty
		Error          string
	}{basePageData: s.newBasePageData(r)}

	holds, err := s.Screening.ListHolds(r.Context(), "OPEN")
	if err != nil {
		data.Error = "listing C3 holds: " + err.Error()
		s.Templates.Render(w, "aml_exposure", data)
		return
	}

	order := make(map[string]int) // sender address -> index into data.Counterparties
	for _, h := range holds {
		address := "(unknown -- order lookup failed)"
		state := ""
		if ord, err := s.Ledger.GetOrder(r.Context(), h.ExternalID); err == nil {
			state = ord.State
			if ord.SenderAddress != "" {
				address = ord.SenderAddress
			} else {
				address = "(order has no sender_address yet)"
			}
		}

		idx, ok := order[address]
		if !ok {
			idx = len(data.Counterparties)
			order[address] = idx
			cp := amlCounterparty{Address: address}
			if results, err := s.Screening.GetScreeningResults(r.Context(), address); err == nil {
				for _, res := range results {
					if !res.Flagged {
						continue
					}
					cp.VerdictKnown = true
					cp.Provider = res.ProviderName
					cp.RiskScore = res.RiskScore
					cp.CheckedAt = res.CheckedAt.String()
					for i, code := range res.ReasonCodes {
						if i > 0 {
							cp.ReasonCodes += ", "
						}
						cp.ReasonCodes += code
					}
					break // newest flagged result is enough for this summary view
				}
			}
			data.Counterparties = append(data.Counterparties, cp)
		}

		depositAddress := ""
		if wa, err := s.Watcher.GetAddress(r.Context(), h.OrderID); err == nil {
			depositAddress = wa.Address
		}
		data.Counterparties[idx].Orders = append(data.Counterparties[idx].Orders, amlLinkedOrder{
			ExternalID: h.ExternalID, State: state, DepositAddress: depositAddress, ReasonCode: h.ReasonCode,
		})
	}

	s.Templates.Render(w, "aml_exposure", data)
}
