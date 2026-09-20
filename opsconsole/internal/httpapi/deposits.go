package httpapi

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

const depositsWatchingContent = `
<div class="page-head"><h1>Deposit watch</h1></div>
<p class="helptext">
  Every deposit address still WATCHING or FUNDED (not yet final). If the
  background scanner missed a real deposit -- an RPC hiccup, a provider
  outage -- confirm it by hand below: this looks up the real transaction
  on-chain and feeds it through the exact same crediting path an
  automatically-detected deposit uses. It cannot skip finality or credit
  something that isn't really there.
</p>
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
{{ if .Flash }}<div class="flash {{ if .FlashError }}flash-error{{ else }}flash-ok{{ end }}">{{ if .FlashError }}` + iconAlert + `{{ else }}` + iconCheck + `{{ end }}<span>{{ .Flash }}</span></div>{{ end }}
<div class="table-wrap">
<table>
<tr><th>Order</th><th>Address</th><th>Status</th><th>Confirm a deposit</th></tr>
{{ range .Addresses }}
<tr>
  <td>{{ .OrderID }}</td>
  <td class="mono">{{ .Address }}</td>
  <td><span class="badge {{ if eq .Status "FUNDED" }}badge-success{{ else }}badge-warning{{ end }}">{{ .Status }}</span></td>
  <td>
    <form class="inline" method="post" action="/deposits/{{ .OrderID }}/confirm">
      <div class="actions">
        <input type="text" name="tx_hash" placeholder="0x..." style="width:280px" required>
        <button class="btn btn-sm" type="submit">Confirm</button>
      </div>
    </form>
  </td>
</tr>
{{ else }}
<tr><td colspan="4"><div class="empty-state">Nothing WATCHING or FUNDED right now.</div></td></tr>
{{ end }}
</table>
</div>
`

type depositRow struct {
	OrderID int64
	Address string
	Status  string
}

type depositsWatchingPageData struct {
	basePageData
	Addresses  []depositRow
	Error      string
	Flash      string
	FlashError bool
}

func (s *Server) renderDepositsWatching(w http.ResponseWriter, r *http.Request, flash string, flashErr bool) {
	data := depositsWatchingPageData{basePageData: s.newBasePageData(r), Flash: flash, FlashError: flashErr}
	all, err := s.Watcher.ListAddresses(r.Context(), 500)
	if err != nil {
		data.Error = err.Error()
		s.Templates.Render(w, "deposits_watching", data)
		return
	}
	// C2's own GET /v1/addresses takes no status filter -- filtered
	// here, same pattern the Sweep and Wallets pages already use.
	for _, a := range all {
		if a.Status == "WATCHING" || a.Status == "FUNDED" {
			data.Addresses = append(data.Addresses, depositRow{OrderID: a.OrderID, Address: a.Address, Status: a.Status})
		}
	}
	s.Templates.Render(w, "deposits_watching", data)
}

func (s *Server) getDepositsWatching(w http.ResponseWriter, r *http.Request) {
	s.renderDepositsWatching(w, r, "", false)
}

// postConfirmDeposit calls C2's own new confirm-deposit route --
// audit-logged before the call (invariant 3), and shows exactly what
// C2 returned, success or failure, never a generic "submitted".
func (s *Server) postConfirmDeposit(w http.ResponseWriter, r *http.Request) {
	orderID, err := strconv.ParseInt(chi.URLParam(r, "order_id"), 10, 64)
	if err != nil {
		s.renderDepositsWatching(w, r, "invalid order id", true)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderDepositsWatching(w, r, "malformed form submission", true)
		return
	}
	txHash := r.FormValue("tx_hash")
	if txHash == "" {
		s.renderDepositsWatching(w, r, "tx_hash is required", true)
		return
	}

	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "deposits.confirm", "order:"+strconv.FormatInt(orderID, 10), map[string]any{"tx_hash": txHash})

	result, err := s.Watcher.ConfirmDeposit(r.Context(), orderID, txHash)
	if err != nil {
		s.renderDepositsWatching(w, r, "confirming: "+err.Error(), true)
		return
	}
	s.renderDepositsWatching(w, r, "Order "+strconv.FormatInt(result.OrderID, 10)+": accepted at block "+strconv.FormatUint(result.BlockNumber, 10)+" -- will credit once finality confirms it, same as any other deposit.", false)
}
