package httpapi

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

const screeningTabs = `
<div class="tabs">
  <a class="tab {{ if eq .ScreeningTab "holds" }}active{{ end }}" href="/screening/holds">Holds</a>
  <a class="tab {{ if eq .ScreeningTab "rescreen" }}active{{ end }}" href="/screening/rescreen-flags">Rescreen flags</a>
  <a class="tab {{ if eq .ScreeningTab "results" }}active{{ end }}" href="/screening/results">Results</a>
  <a class="tab {{ if eq .ScreeningTab "queue" }}active{{ end }}" href="/screening/queue">Queue</a>
</div>
`

const screeningHoldsContent = `
<div class="page-head"><h1>Screening holds</h1></div>
` + screeningTabs + `
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
<div class="table-wrap">
<table>
<tr><th>ID</th><th>Order</th><th>External ID</th><th>Reason</th><th>Opened</th><th>Status</th><th></th></tr>
{{ range .Holds }}
<tr>
  <td class="mono">{{ .ID }}</td><td>{{ .OrderID }}</td><td>{{ .ExternalID }}</td><td><span class="badge badge-neutral">{{ .ReasonCode }}</span></td>
  <td class="mono">{{ .OpenedAt }}</td>
  <td><span class="badge {{ if eq .Status "OPEN" }}badge-warning{{ else }}badge-success{{ end }}">{{ .Status }}</span></td>
  <td>{{ if eq .Status "OPEN" }}
    <div class="actions">
    <form class="inline" method="post" action="/screening/holds/{{ .ID }}/release">
      <div class="actions">
        <input type="text" name="note" placeholder="note (optional)" style="width:140px">
        <button class="btn btn-sm" type="submit">Release</button>
      </div>
    </form>
    <form class="inline" method="post" action="/screening/holds/{{ .ID }}/reject">
      <div class="actions">
        <input type="text" name="note" placeholder="note (optional)" style="width:140px">
        <button class="btn btn-sm btn-danger" type="submit">Reject</button>
      </div>
    </form>
    </div>
  {{ end }}</td>
</tr>
{{ else }}
<tr><td colspan="7"><div class="empty-state">No holds right now.</div></td></tr>
{{ end }}
</table>
</div>
`

const dispatcherSlotsContent = `
<div class="page-head"><h1>Dispatcher slots</h1></div>
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
<div class="table-wrap">
<table>
<tr><th>ID</th><th>Address</th><th>Status</th><th>Balance</th><th>Tx count</th><th></th></tr>
{{ range .Slots }}
<tr>
  <td>{{ .ID }}</td><td class="mono">{{ .TronAddress }}</td>
  <td><span class="badge {{ if eq .Status "ACTIVE" }}badge-success{{ else if eq .Status "RETIRED" }}badge-neutral{{ else }}badge-warning{{ end }}">{{ .Status }}</span></td>
  <td>{{ .Balance }}</td><td>{{ .TxCount }}</td>
  <td>{{ if eq .Status "ACTIVE" }}
    <form class="inline" method="post" action="/dispatcher/slots/{{ .ID }}/retire" onsubmit="return confirm('This is a manual override of the normal cap-triggered rotation. Retire this slot?');">
      <div class="actions">
        <label style="display:inline;margin:0;font-weight:400;color:var(--text-muted)"><input type="checkbox" name="immediate" value="true" style="width:auto;vertical-align:middle"> immediate</label>
        <button class="btn btn-sm btn-danger" type="submit">Retire</button>
      </div>
    </form>
  {{ end }}</td>
</tr>
{{ else }}
<tr><td colspan="6"><div class="empty-state">No slots registered.</div></td></tr>
{{ end }}
</table>
</div>
`

type holdRow struct {
	ID, OrderID int64
	ExternalID  string
	ReasonCode  string
	OpenedAt    string
	Status      string
}

type screeningHoldsPageData struct {
	basePageData
	ScreeningTab string
	Holds        []holdRow
	Error        string
}

func (s *Server) getScreeningHolds(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "OPEN"
	}
	data := screeningHoldsPageData{basePageData: s.newBasePageData(r), ScreeningTab: "holds"}
	holds, err := s.Screening.ListHolds(r.Context(), status)
	if err != nil {
		data.Error = err.Error()
	}
	for _, h := range holds {
		data.Holds = append(data.Holds, holdRow{
			ID: h.ID, OrderID: h.OrderID, ExternalID: h.ExternalID, ReasonCode: h.ReasonCode,
			OpenedAt: h.OpenedAt.String(), Status: h.Status,
		})
	}
	s.Templates.Render(w, "screening_holds", data)
}

func (s *Server) postScreeningHoldRelease(w http.ResponseWriter, r *http.Request) {
	s.resolveHold(w, r, true)
}

func (s *Server) postScreeningHoldReject(w http.ResponseWriter, r *http.Request) {
	s.resolveHold(w, r, false)
}

func (s *Server) resolveHold(w http.ResponseWriter, r *http.Request, release bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid hold id", http.StatusBadRequest)
		return
	}
	_ = r.ParseForm()
	note := r.FormValue("note")
	sess, _ := sessionFromContext(r.Context())

	action := "screening.hold.release"
	if !release {
		action = "screening.hold.reject"
	}
	_ = s.Audit.Write(sess.Username, action, "hold:"+strconv.FormatInt(id, 10), map[string]any{"note": note})

	if release {
		err = s.Screening.ReleaseHold(r.Context(), id, sess.DisplayName, note)
	} else {
		err = s.Screening.RejectHold(r.Context(), id, sess.DisplayName, note)
	}
	if err != nil {
		http.Error(w, "opsconsole: "+err.Error(), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/screening/holds", http.StatusFound)
}

type slotRow struct {
	ID          int
	TronAddress string
	Status      string
	Balance     string
	TxCount     int64
}

type dispatcherSlotsPageData struct {
	basePageData
	Slots []slotRow
	Error string
}

func (s *Server) getDispatcherSlots(w http.ResponseWriter, r *http.Request) {
	data := dispatcherSlotsPageData{basePageData: s.newBasePageData(r)}
	slots, err := s.Dispatcher.ListSlots(r.Context())
	if err != nil {
		data.Error = err.Error()
	}
	for _, sl := range slots {
		data.Slots = append(data.Slots, slotRow{ID: sl.ID, TronAddress: sl.TronAddress, Status: sl.Status, Balance: sl.Balance, TxCount: sl.TxCount})
	}
	s.Templates.Render(w, "dispatcher_slots", data)
}

func (s *Server) postDispatcherSlotRetire(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid slot id", http.StatusBadRequest)
		return
	}
	_ = r.ParseForm()
	immediate := r.FormValue("immediate") == "true"
	sess, _ := sessionFromContext(r.Context())

	_ = s.Audit.Write(sess.Username, "dispatcher.slot.retire", "slot:"+strconv.Itoa(id), map[string]any{"immediate": immediate})
	if err := s.Dispatcher.RetireSlot(r.Context(), id, immediate); err != nil {
		http.Error(w, "opsconsole: "+err.Error(), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/dispatcher/slots", http.StatusFound)
}
