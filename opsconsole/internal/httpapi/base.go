package httpapi

import (
	"net/http"

	"opsconsole/internal/session"
)

// basePageData is embedded by every page's own data struct so the shared
// layout template (templates.go) can always find .Session, .Halted, and
// .HaltReason regardless of which page is rendering -- html/template
// errors on a field a data value doesn't have, so every page needs these
// three, not just the ones that happen to care about halt state.
type basePageData struct {
	Session    *session.Session
	Halted     bool
	HaltReason string
}

// newBasePageData reads the caller's session from context and, best-
// effort, C1's current halt state -- the banner (OC.2's own doc comment:
// "the one piece of state urgent enough to surface everywhere") degrades
// silently to "not shown" rather than failing the whole page if the
// ledger is unreachable, matching invariant 4's own "one dead service
// degrades that one panel, never the whole page" posture applied here to
// a banner instead of a card.
func (s *Server) newBasePageData(r *http.Request) basePageData {
	data := basePageData{}
	if sess, ok := sessionFromContext(r.Context()); ok {
		data.Session = &sess
	}
	if halt, err := s.Ledger.GetHaltState(r.Context()); err == nil {
		data.Halted = halt.Halted
		data.HaltReason = halt.Reason
	}
	return data
}
