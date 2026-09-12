package httpapi

import (
	"net/http"
)

const ledgerHaltContent = `
<h1>Ledger halt</h1>
{{ if .Flash }}<div class="flash {{ if .FlashError }}flash-error{{ else }}flash-ok{{ end }}">{{ .Flash }}</div>{{ end }}
{{ if .Halt.Halted }}
  <div class="card">
    <p><strong>Halted.</strong> Reason: {{ .Halt.Reason }}</p>
    <form method="post" action="/ledger/halt/clear">
      <p><input type="submit" value="Clear halt"></p>
    </form>
  </div>
{{ else }}
  <div class="card">
    <p>Not halted.</p>
    <form method="post" action="/ledger/halt/set">
      <label>Reason</label>
      <input type="text" name="reason" required>
      <p><button class="danger" type="submit">Set halt</button></p>
    </form>
  </div>
{{ end }}
`

type ledgerHaltPageData struct {
	basePageData
	Halt       haltView
	Flash      string
	FlashError bool
}

type haltView struct {
	Halted bool
	Reason string
}

func (s *Server) renderLedgerHalt(w http.ResponseWriter, r *http.Request, flash string, flashErr bool) {
	halt, err := s.Ledger.GetHaltState(r.Context())
	if err != nil {
		s.Templates.Render(w, "ledger_halt", ledgerHaltPageData{
			basePageData: s.newBasePageData(r), Flash: "reading halt state: " + err.Error(), FlashError: true,
		})
		return
	}
	s.Templates.Render(w, "ledger_halt", ledgerHaltPageData{
		basePageData: s.newBasePageData(r),
		Halt:         haltView{Halted: halt.Halted, Reason: halt.Reason},
		Flash:        flash, FlashError: flashErr,
	})
}

func (s *Server) getLedgerHalt(w http.ResponseWriter, r *http.Request) {
	s.renderLedgerHalt(w, r, "", false)
}

func (s *Server) postLedgerHaltSet(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderLedgerHalt(w, r, "malformed form submission", true)
		return
	}
	reason := r.FormValue("reason")
	sess, _ := sessionFromContext(r.Context())

	_ = s.Audit.Write(sess.Username, "ledger.halt.set", "ledger", map[string]any{"reason": reason})
	if err := s.Ledger.SetHalt(r.Context(), "set", reason, sess.DisplayName); err != nil {
		s.renderLedgerHalt(w, r, "setting halt: "+err.Error(), true)
		return
	}
	s.renderLedgerHalt(w, r, "Halt set.", false)
}

func (s *Server) postLedgerHaltClear(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFromContext(r.Context())

	_ = s.Audit.Write(sess.Username, "ledger.halt.clear", "ledger", nil)
	if err := s.Ledger.SetHalt(r.Context(), "clear", "", sess.DisplayName); err != nil {
		s.renderLedgerHalt(w, r, "clearing halt: "+err.Error(), true)
		return
	}
	s.renderLedgerHalt(w, r, "Halt cleared.", false)
}
