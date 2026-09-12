package httpapi

import (
	"net/http"

	"opsconsole/internal/auditlog"
)

const auditContent = `
<h1>Audit log</h1>
{{ if .Error }}<div class="flash flash-error">{{ .Error }}</div>{{ end }}
<table>
<tr><th>Time</th><th>Operator</th><th>Action</th><th>Target</th><th>Detail</th></tr>
{{ range .Entries }}
<tr><td>{{ .Time }}</td><td>{{ .Operator }}</td><td>{{ .Action }}</td><td>{{ .Target }}</td><td>{{ .Detail }}</td></tr>
{{ end }}
</table>
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
