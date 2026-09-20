package httpapi

import (
	"net/http"

	"opsconsole/internal/auditlog"
)

const auditContent = `
<div class="page-head"><h1>Audit log</h1></div>
<p class="helptext">Every write action this console makes, logged before the downstream call -- an operator convenience view, bounded to the last 500 lines, not a queryable log store.</p>
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
<div class="table-wrap">
<table>
<tr><th>Time</th><th>Operator</th><th>Action</th><th>Target</th><th>Detail</th></tr>
{{ range .Entries }}
<tr><td class="mono">{{ .Time }}</td><td>{{ .Operator }}</td><td><span class="badge badge-neutral">{{ .Action }}</span></td><td class="mono">{{ .Target }}</td><td class="mono" style="max-width:360px;overflow-wrap:anywhere">{{ .Detail }}</td></tr>
{{ else }}
<tr><td colspan="5"><div class="empty-state">No audit entries yet.</div></td></tr>
{{ end }}
</table>
</div>
`

type auditPageData struct {
	basePageData
	Entries []auditlog.Entry
	Error   string
}

// getAudit is OC.8's own read view -- reverse-chronological, bounded to
// the last 500 lines. An operator convenience page, not a queryable log
// store.
func (s *Server) getAudit(w http.ResponseWriter, r *http.Request) {
	data := auditPageData{basePageData: s.newBasePageData(r)}
	entries, err := auditlog.Tail(s.AuditPath, 500)
	if err != nil {
		data.Error = err.Error()
	} else {
		for i := len(entries) - 1; i >= 0; i-- {
			data.Entries = append(data.Entries, entries[i])
		}
	}
	s.Templates.Render(w, "audit", data)
}
