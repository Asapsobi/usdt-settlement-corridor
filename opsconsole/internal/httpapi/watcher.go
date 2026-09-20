package httpapi

import (
	"net/http"
	"strconv"
)

const watcherTabs = `
<div class="tabs">
  <a class="tab {{ if eq .WatcherTab "cursor" }}active{{ end }}" href="/watcher/cursor">Cursor</a>
  <a class="tab {{ if eq .WatcherTab "sweep" }}active{{ end }}" href="/watcher/sweep">Sweep</a>
  <a class="tab {{ if eq .WatcherTab "addresses" }}active{{ end }}" href="/watcher/addresses">Addresses</a>
  <a class="tab {{ if eq .WatcherTab "orphaned" }}active{{ end }}" href="/watcher/orphaned-deposits">Orphaned deposits</a>
  <a class="tab {{ if eq .WatcherTab "providers" }}active{{ end }}" href="/watcher/providers">Providers</a>
</div>
`

const watcherCursorContent = `
<div class="page-head"><h1>Watcher cursor</h1></div>
` + watcherTabs + `
{{ if .Flash }}<div class="flash {{ if .FlashError }}flash-error{{ else }}flash-ok{{ end }}">{{ if .FlashError }}` + iconAlert + `{{ else }}` + iconCheck + `{{ end }}<span>{{ .Flash }}</span></div>{{ end }}
<div class="cards" style="margin-bottom:16px;grid-template-columns:repeat(auto-fill,minmax(200px,1fr))">
  <div class="card"><div class="fact-label" style="margin-bottom:6px">last_scanned</div><div style="font-size:22px;font-weight:650;font-variant-numeric:tabular-nums">{{ .Cursor.LastScanned }}</div></div>
  <div class="card"><div class="fact-label" style="margin-bottom:6px">last_candidate_scanned</div><div style="font-size:22px;font-weight:650;font-variant-numeric:tabular-nums">{{ .Cursor.LastCandidateScanned }}</div></div>
  <div class="card"><div class="fact-label" style="margin-bottom:6px">updated_at</div><div class="mono">{{ .Cursor.UpdatedAt }}</div></div>
</div>
<div class="card panel-narrow">
  <h2 style="margin-bottom:10px">Override</h2>
  <p class="field-hint" style="margin:0">Sets both cursor columns at once, validated against the chain's own current tip.</p>
  <form method="post" action="/watcher/cursor">
    <label>last_scanned</label>
    <input type="number" name="last_scanned" required>
    <label>last_candidate_scanned</label>
    <input type="number" name="last_candidate_scanned" required>
    <label>reason</label>
    <input type="text" name="reason" required>
    <p style="margin-top:16px"><button class="btn btn-danger" type="submit">Set cursor</button></p>
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
	WatcherTab string
	Cursor     cursorView
	Flash      string
	FlashError bool
}

func (s *Server) renderWatcherCursor(w http.ResponseWriter, r *http.Request, flash string, flashErr bool) {
	cur, err := s.Watcher.GetCursor(r.Context())
	if err != nil {
		s.Templates.Render(w, "watcher_cursor", watcherCursorPageData{
			basePageData: s.newBasePageData(r), WatcherTab: "cursor", Flash: "reading cursor: " + err.Error(), FlashError: true,
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
		basePageData: s.newBasePageData(r), WatcherTab: "cursor", Cursor: view, Flash: flash, FlashError: flashErr,
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

const watcherSweepContent = `
<div class="page-head"><h1>Sweep</h1></div>
` + watcherTabs + `
<p class="helptext">
  Lists every known deposit address, including already-settled orders --
  a settled order's own BEP20 balance still sits at that address until
  someone sweeps it. Balance is checked on demand (one on-chain read per
  click), never automatically for every row. Sweeping itself always
  happens in your own terminal: this page only ever gives you the exact
  command to run -- it never asks for a seed phrase or private key.
</p>
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
<div class="table-wrap">
<table>
<tr><th>Order</th><th>Address</th><th>Index</th><th>Status</th><th>Balance</th><th></th></tr>
{{ range .Addresses }}
<tr>
  <td>{{ .OrderID }}</td>
  <td class="mono">{{ .Address }}</td>
  <td>{{ .DerivationIndex }}</td>
  <td><span class="badge {{ if eq .Status "RETIRED" }}badge-neutral{{ else if eq .Status "FUNDED" }}badge-success{{ else }}badge-warning{{ end }}">{{ .Status }}</span></td>
  <td>
    {{ if .BalanceChecked }}
      {{ if .BalanceError }}<span class="err" style="color:var(--danger)">{{ .BalanceError }}</span>
      {{ else }}<span class="mono">{{ .BalanceRaw }}</span> raw units{{ end }}
    {{ else }}
      <a class="btn btn-sm" href="/watcher/sweep?check_order_id={{ .OrderID }}">Check balance</a>
    {{ end }}
  </td>
  <td>{{ if .Sweepable }}<span class="mono" style="font-size:12px">go run . -index={{ .DerivationIndex }} -expect-address={{ .Address }}</span>{{ end }}</td>
</tr>
{{ else }}
<tr><td colspan="6"><div class="empty-state">No watched addresses yet.</div></td></tr>
{{ end }}
</table>
</div>
{{ if .SweepCommandFor }}
<div class="card panel-narrow section">
  <h2 style="margin-bottom:10px">Sweep command for order {{ .SweepCommandFor }}</h2>
  <p class="field-hint" style="margin:0 0 10px">Run this in your own terminal, in the sweepbsc directory. It will prompt for the destination address and your seed phrase interactively -- never paste either into this web page.</p>
  <div class="mono" style="background:var(--surface-2);padding:12px;border-radius:8px;overflow-x:auto">go run . -index={{ .SweepCommandIndex }} -expect-address={{ .SweepCommandAddress }}</div>
</div>
{{ end }}
`

type addressRow struct {
	OrderID         int64
	Address         string
	DerivationIndex uint32
	Status          string
	BalanceChecked  bool
	BalanceRaw      string
	BalanceError    string
	Sweepable       bool
}

type watcherSweepPageData struct {
	basePageData
	WatcherTab          string
	Addresses           []addressRow
	Error               string
	SweepCommandFor     int64
	SweepCommandIndex   uint32
	SweepCommandAddress string
}

func (s *Server) getWatcherSweep(w http.ResponseWriter, r *http.Request) {
	data := watcherSweepPageData{basePageData: s.newBasePageData(r), WatcherTab: "sweep"}
	list, err := s.Watcher.ListAddresses(r.Context(), 200)
	if err != nil {
		data.Error = err.Error()
		s.Templates.Render(w, "watcher_sweep", data)
		return
	}

	checkOrderID, _ := strconv.ParseInt(r.URL.Query().Get("check_order_id"), 10, 64)
	for _, a := range list {
		row := addressRow{OrderID: a.OrderID, Address: a.Address, DerivationIndex: a.DerivationIndex, Status: a.Status}
		if checkOrderID != 0 && a.OrderID == checkOrderID {
			row.BalanceChecked = true
			balance, err := s.Watcher.GetAddressBalance(r.Context(), a.OrderID)
			if err != nil {
				row.BalanceError = err.Error()
			} else {
				row.BalanceRaw = balance
				if balance != "" && balance != "0" {
					row.Sweepable = true
					data.SweepCommandFor = a.OrderID
					data.SweepCommandIndex = a.DerivationIndex
					data.SweepCommandAddress = a.Address
				}
			}
		}
		data.Addresses = append(data.Addresses, row)
	}
	s.Templates.Render(w, "watcher_sweep", data)
}
