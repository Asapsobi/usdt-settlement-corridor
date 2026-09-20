package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// This file closes the C3 (screening) routes OC.6 (holds only) and
// OC.13 (read-only AML exposure) don't reach: rescreen-flag review,
// screening-result invalidation, and the discovery-queue depth view.

const screeningRescreenContent = `
<div class="page-head"><h1>Rescreen flags</h1></div>
` + screeningTabs + `
<p class="helptext">Recorded when a later screening verdict for an address diverges from the verdict its order was originally screened under. Resolving one is purely a record of a human decision -- C3 itself never acts on the resolution (mechanism, not policy).</p>
{{ if .Flash }}<div class="flash {{ if .FlashError }}flash-error{{ else }}flash-ok{{ end }}">{{ if .FlashError }}` + iconAlert + `{{ else }}` + iconCheck + `{{ end }}<span>{{ .Flash }}</span></div>{{ end }}
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
<div class="table-wrap">
<table>
<tr><th>ID</th><th>Order</th><th>State then</th><th>Previous verdict</th><th>New verdict</th><th>Detected</th><th>Resolution</th><th></th></tr>
{{ range .Flags }}
<tr>
  <td class="mono">{{ .ID }}</td>
  <td class="mono"><a href="/orders/{{ .ExternalID }}">{{ .ExternalID }}</a></td>
  <td>{{ .OrderStateAtDetection }}</td>
  <td class="mono">{{ .PreviousVerdictID }}</td>
  <td class="mono">{{ .NewVerdictID }}</td>
  <td class="mono">{{ .DetectedAt }}</td>
  <td>{{ if .Resolution }}<span class="badge badge-success">{{ .Resolution }}</span>{{ else }}<span class="badge badge-warning">open</span>{{ end }}</td>
  <td>{{ if not .Resolution }}
    <form class="inline" method="post" action="/screening/rescreen-flags/{{ .ID }}/resolve">
      <div class="actions">
        <input type="text" name="resolution" placeholder="resolution" required style="width:180px">
        <button class="btn btn-sm" type="submit">Resolve</button>
      </div>
    </form>
  {{ end }}</td>
</tr>
{{ else }}
<tr><td colspan="8"><div class="empty-state">No rescreen flags.</div></td></tr>
{{ end }}
</table>
</div>
`

const screeningResultsContent = `
<div class="page-head"><h1>Screening results</h1></div>
` + screeningTabs + `
<p class="helptext">C3 has no route to browse every cached result -- GET /v1/screening-results requires a sender_address you already know (same real constraint <a href="/aml/exposure">AML exposure</a> works around, since this page's own action -- invalidating a cached result -- belongs here, not on that read-only view).</p>
{{ if .Flash }}<div class="flash {{ if .FlashError }}flash-error{{ else }}flash-ok{{ end }}">{{ if .FlashError }}` + iconAlert + `{{ else }}` + iconCheck + `{{ end }}<span>{{ .Flash }}</span></div>{{ end }}
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
<div class="card panel-narrow">
  <form method="get" action="/screening/results">
    <label>sender_address</label>
    <input type="text" name="sender_address" value="{{ .QueryAddress }}" placeholder="0x...">
    <p style="margin-top:16px"><button class="btn btn-primary" type="submit">Look up</button></p>
  </form>
</div>
{{ if .QueryAddress }}
<div class="table-wrap section">
<table>
<tr><th>ID</th><th>Provider</th><th>Risk score</th><th>Flagged</th><th>Checked</th><th></th></tr>
{{ range .Results }}
<tr>
  <td class="mono">{{ .ID }}</td>
  <td>{{ .ProviderName }}</td>
  <td class="mono">{{ .RiskScore }}</td>
  <td>{{ if .Flagged }}<span class="badge badge-danger">flagged</span>{{ else }}<span class="badge badge-neutral">clear</span>{{ end }}</td>
  <td class="mono">{{ .CheckedAt }}</td>
  <td>
    <details>
      <summary class="btn btn-sm btn-danger" style="display:inline-block;cursor:pointer">Invalidate</summary>
      <form method="post" action="/screening/results/{{ .ID }}/invalidate" style="margin-top:10px;min-width:280px">
        <div class="flash flash-error" style="margin-bottom:10px">` + iconAlert + `<span>Invalidates every cached result for {{ $.QueryAddress }} + {{ .ProviderName }} -- not just this one row. Nothing is deleted, and this does not force a re-screen or change any order's already-recorded state; it only means the next check for this pair skips the cache.</span></div>
        <label>reason</label>
        <input type="text" name="reason" required>
        <p style="margin-top:12px"><button class="btn btn-sm btn-danger" type="submit">Confirm invalidate</button></p>
      </form>
    </details>
  </td>
</tr>
{{ else }}
<tr><td colspan="6"><div class="empty-state">No screening results for that address.</div></td></tr>
{{ end }}
</table>
</div>
{{ end }}
`

const screeningQueueContent = `
<div class="page-head"><h1>Screening queue</h1></div>
` + screeningTabs + `
<p class="helptext">GET /v1/system/queue -- the discovery/re-screen loop's own queue depth. Closes a real blind spot: whether the screening pipeline is keeping up.</p>
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
<div class="cards" style="grid-template-columns:repeat(auto-fill,minmax(180px,1fr))">
{{ range $status, $depth := .DepthByStatus }}
  <div class="card"><div class="fact-label" style="margin-bottom:6px">{{ $status }}</div><div style="font-size:22px;font-weight:650;font-variant-numeric:tabular-nums">{{ $depth }}</div></div>
{{ else }}
  <div class="empty-state">Queue is empty.</div>
{{ end }}
</div>
{{ if .OldestPendingAgeSeconds }}<p class="field-hint" style="margin-top:14px">oldest_pending_age_seconds: <span class="mono">{{ .OldestPendingAgeSeconds }}</span></p>{{ end }}
`

type rescreenFlagRow struct {
	ID, OrderID                                               int64
	ExternalID, OrderStateAtDetection, DetectedAt, Resolution string
	PreviousVerdictID, NewVerdictID                           int64
}

type screeningRescreenPageData struct {
	basePageData
	ScreeningTab string
	Flags        []rescreenFlagRow
	Flash        string
	FlashError   bool
	Error        string
}

func (s *Server) renderScreeningRescreen(w http.ResponseWriter, r *http.Request, flash string, flashErr bool) {
	data := screeningRescreenPageData{basePageData: s.newBasePageData(r), ScreeningTab: "rescreen", Flash: flash, FlashError: flashErr}
	flags, err := s.Screening.ListRescreenFlags(r.Context(), nil)
	if err != nil {
		data.Error = err.Error()
		s.Templates.Render(w, "screening_rescreen", data)
		return
	}
	for _, f := range flags {
		row := rescreenFlagRow{
			ID: f.ID, OrderID: f.OrderID, ExternalID: f.ExternalID, OrderStateAtDetection: f.OrderStateAtDetection,
			PreviousVerdictID: f.PreviousVerdictID, NewVerdictID: f.NewVerdictID, DetectedAt: f.DetectedAt.String(),
		}
		if f.Resolution != nil {
			row.Resolution = *f.Resolution
		}
		data.Flags = append(data.Flags, row)
	}
	s.Templates.Render(w, "screening_rescreen", data)
}

func (s *Server) getScreeningRescreenFlags(w http.ResponseWriter, r *http.Request) {
	s.renderScreeningRescreen(w, r, "", false)
}

func (s *Server) postScreeningRescreenFlagResolve(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid flag id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderScreeningRescreen(w, r, "malformed form submission", true)
		return
	}
	resolution := strings.TrimSpace(r.FormValue("resolution"))
	if resolution == "" {
		s.renderScreeningRescreen(w, r, "resolution is required", true)
		return
	}

	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "screening.rescreen_flag.resolve", "rescreen-flag:"+strconv.FormatInt(id, 10), map[string]any{"resolution": resolution})

	if _, err := s.Screening.ResolveRescreenFlag(r.Context(), id, resolution, sess.DisplayName); err != nil {
		s.renderScreeningRescreen(w, r, "resolving: "+err.Error(), true)
		return
	}
	s.renderScreeningRescreen(w, r, "Rescreen flag resolved.", false)
}

type screeningResultRow struct {
	ID           int64
	ProviderName string
	RiskScore    float64
	Flagged      bool
	CheckedAt    string
}

type screeningResultsPageData struct {
	basePageData
	ScreeningTab string
	QueryAddress string
	Results      []screeningResultRow
	Flash        string
	FlashError   bool
	Error        string
}

func (s *Server) renderScreeningResults(w http.ResponseWriter, r *http.Request, address, flash string, flashErr bool) {
	data := screeningResultsPageData{basePageData: s.newBasePageData(r), ScreeningTab: "results", QueryAddress: address, Flash: flash, FlashError: flashErr}
	if address != "" {
		results, err := s.Screening.GetScreeningResults(r.Context(), address)
		if err != nil {
			data.Error = err.Error()
		}
		for _, res := range results {
			data.Results = append(data.Results, screeningResultRow{ID: res.ID, ProviderName: res.ProviderName, RiskScore: res.RiskScore, Flagged: res.Flagged, CheckedAt: res.CheckedAt.String()})
		}
	}
	s.Templates.Render(w, "screening_results", data)
}

func (s *Server) getScreeningResults(w http.ResponseWriter, r *http.Request) {
	s.renderScreeningResults(w, r, r.URL.Query().Get("sender_address"), "", false)
}

// postScreeningResultInvalidate calls C3's own real POST
// /v1/screening-results/{id}/invalidate. See
// opclient.InvalidateScreeningResult's own doc comment for the real,
// verified consequence (the whole provider+address pair, not just this
// row) -- the confirm copy in screeningResultsContent states that
// plainly, and reason is required server-side before this handler
// calls C3 at all.
func (s *Server) postScreeningResultInvalidate(w http.ResponseWriter, r *http.Request) {
	idParam := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idParam, 10, 64)
	if err != nil {
		http.Error(w, "invalid result id", http.StatusBadRequest)
		return
	}
	address := r.URL.Query().Get("sender_address")
	if err := r.ParseForm(); err != nil {
		s.renderScreeningResults(w, r, address, "malformed form submission", true)
		return
	}
	if address == "" {
		address = r.FormValue("sender_address")
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		s.renderScreeningResults(w, r, address, "reason is required", true)
		return
	}

	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "screening.result.invalidate", "screening-result:"+idParam, map[string]any{"reason": reason, "sender_address": address})

	if err := s.Screening.InvalidateScreeningResult(r.Context(), id, reason, sess.DisplayName); err != nil {
		s.renderScreeningResults(w, r, address, "invalidating: "+err.Error(), true)
		return
	}
	s.renderScreeningResults(w, r, address, "Result "+idParam+" invalidated -- every cached result for this provider+address pair is no longer trusted.", false)
}

type screeningQueuePageData struct {
	basePageData
	ScreeningTab            string
	DepthByStatus           map[string]int
	OldestPendingAgeSeconds *float64
	Error                   string
}

func (s *Server) getScreeningQueue(w http.ResponseWriter, r *http.Request) {
	data := screeningQueuePageData{basePageData: s.newBasePageData(r), ScreeningTab: "queue"}
	stats, err := s.Screening.GetQueue(r.Context())
	if err != nil {
		data.Error = err.Error()
		s.Templates.Render(w, "screening_queue", data)
		return
	}
	data.DepthByStatus = stats.DepthByStatus
	data.OldestPendingAgeSeconds = stats.OldestPendingAgeSeconds
	s.Templates.Render(w, "screening_queue", data)
}
