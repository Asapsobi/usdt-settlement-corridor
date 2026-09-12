package httpapi

import (
	"net/http"
	"strconv"
)

const watcherCursorContent = `
<h1>Watcher cursor</h1>
{{ if .Flash }}<div class="flash {{ if .FlashError }}flash-error{{ else }}flash-ok{{ end }}">{{ .Flash }}</div>{{ end }}
<div class="card">
  <p>last_scanned: {{ .Cursor.LastScanned }}</p>
  <p>last_candidate_scanned: {{ .Cursor.LastCandidateScanned }}</p>
  <p>updated_at: {{ .Cursor.UpdatedAt }}</p>
</div>
<div class="card">
  <form method="post" action="/watcher/cursor">
    <label>last_scanned</label>
    <input type="number" name="last_scanned" required>
    <label>last_candidate_scanned</label>
    <input type="number" name="last_candidate_scanned" required>
    <label>reason</label>
    <input type="text" name="reason" required>
    <p><button class="danger" type="submit">Set cursor</button></p>
  </form>
</div>
`

type cursorView struct {
	LastScanned          int64
	LastCandidateScanned string
	UpdatedAt            string
}

type watcherCursorPageData struct {
	basePageData
	Cursor     cursorView
	Flash      string
	FlashError bool
}

func (s *Server) renderWatcherCursor(w http.ResponseWriter, r *http.Request, flash string, flashErr bool) {
	cur, err := s.Watcher.GetCursor(r.Context())
	if err != nil {
		s.Templates.Render(w, "watcher_cursor", watcherCursorPageData{
			basePageData: s.newBasePageData(r), Flash: "reading cursor: " + err.Error(), FlashError: true,
		})
		return
	}
	view := cursorView{LastScanned: cur.LastScanned, LastCandidateScanned: "(none)"}
	if cur.LastCandidateScanned != nil {
		view.LastCandidateScanned = strconv.FormatInt(*cur.LastCandidateScanned, 10)
	}
	if cur.UpdatedAt != nil {
		view.UpdatedAt = cur.UpdatedAt.String()
	}
	s.Templates.Render(w, "watcher_cursor", watcherCursorPageData{
		basePageData: s.newBasePageData(r), Cursor: view, Flash: flash, FlashError: flashErr,
	})
}

func (s *Server) getWatcherCursor(w http.ResponseWriter, r *http.Request) {
	s.renderWatcherCursor(w, r, "", false)
}

func (s *Server) postWatcherCursor(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderWatcherCursor(w, r, "malformed form submission", true)
		return
	}
	lastScanned, err1 := strconv.ParseInt(r.FormValue("last_scanned"), 10, 64)
	lastCandidate, err2 := strconv.ParseInt(r.FormValue("last_candidate_scanned"), 10, 64)
	reason := r.FormValue("reason")
	if err1 != nil || err2 != nil || reason == "" {
		s.renderWatcherCursor(w, r, "last_scanned and last_candidate_scanned must be integers, reason is required", true)
		return
	}

	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "watcher.cursor.set", "watcher", map[string]any{
		"last_scanned": lastScanned, "last_candidate_scanned": lastCandidate, "reason": reason,
	})
	if _, err := s.Watcher.SetCursor(r.Context(), lastScanned, lastCandidate, reason); err != nil {
		s.renderWatcherCursor(w, r, "setting cursor: "+err.Error(), true)
		return
	}
	s.renderWatcherCursor(w, r, "Cursor updated.", false)
}
