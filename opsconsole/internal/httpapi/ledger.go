package httpapi

import (
	"net/http"
)

const ledgerHaltContent = `
<div class="page-head"><h1>Ledger</h1></div>
` + ledgerTabs + `
{{ if .Flash }}<div class="flash {{ if .FlashError }}flash-error{{ else }}flash-ok{{ end }}">{{ if .FlashError }}` + iconAlert + `{{ else }}` + iconCheck + `{{ end }}<span>{{ .Flash }}</span></div>{{ end }}
<div class="card panel-narrow">
{{ if .Halt.Halted }}
  <div class="card-head"><span class="badge badge-danger"><span class="dot dot-red"></span>Halted</span></div>
  <p style="color:var(--text-muted)">Reason: <strong style="color:var(--text)">{{ .Halt.Reason }}</strong></p>
  <form method="post" action="/ledger/halt/clear">
    <button class="btn btn-primary" type="submit">Clear halt</button>
  </form>
{{ else }}
  <div class="card-head"><span class="badge badge-success"><span class="dot dot-green"></span>Not halted</span></div>
  <p class="field-hint" style="margin:0 0 4px">Setting a halt stops the ledger from accepting new writes -- visible as a banner on every page until cleared.</p>
  <form method="post" action="/ledger/halt/set">
    <label>Reason</label>
    <input type="text" name="reason" required>
    <p style="margin-top:16px"><button class="btn btn-danger" type="submit">Set halt</button></p>
  </form>
{{ end }}
</div>
`

type ledgerHaltPageData struct {
	basePageData
	LedgerTab  string
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
			basePageData: s.newBasePageData(r), LedgerTab: "halt", Flash: "reading halt state: " + err.Error(), FlashError: true,
		})
		return
	}
	s.Templates.Render(w, "ledger_halt", ledgerHaltPageData{
		basePageData: s.newBasePageData(r), LedgerTab: "halt",
		Halt:  haltView{Halted: halt.Halted, Reason: halt.Reason},
		Flash: flash, FlashError: flashErr,
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
