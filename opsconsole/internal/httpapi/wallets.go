package httpapi

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"opsconsole/internal/opclient"
)

const walletsContent = `
<div class="page-head"><h1>Wallets</h1></div>
<p class="helptext">
  Every wallet this system actually knows about, by role. No seed or
  private-key column exists here -- not redacted, not masked: for a TRON
  slot the key materially cannot be retrieved at all (signed by S1, never
  exported), and a BSC deposit address is derived from a public key only.
  See <code>docs/03-build/admin-panel-build-prompts.md</code>'s own
  "Read this second."
</p>
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}

<h2 style="font-size:15px;margin:20px 0 10px">TRON payout slots (C5) -- sweepable via S1</h2>
<div class="table-wrap">
<table>
<tr><th>Address</th><th>Role</th><th>Status</th><th>Balance</th><th></th></tr>
{{ range .Slots }}
<tr>
  <td class="mono">{{ .TronAddress }}</td>
  <td><span class="pill-sm core">tron_slot</span></td>
  <td><span class="badge {{ if eq .Status "ACTIVE" }}badge-success{{ else if eq .Status "RETIRED" }}badge-neutral{{ else }}badge-warning{{ end }}">{{ .Status }}</span></td>
  <td class="mono">{{ .Balance }}</td>
  <td>{{ if eq .Status "ACTIVE" }}<a class="btn btn-sm" href="/wallets/{{ .ID }}/sweep">Sweep</a>{{ end }}</td>
</tr>
{{ else }}
<tr><td colspan="5"><div class="empty-state">No slots registered.</div></td></tr>
{{ end }}
</table>
</div>

<h2 style="font-size:15px;margin:24px 0 10px">BSC deposit addresses (C2) -- receive-only, no sweep action here</h2>
<p class="field-hint" style="margin:0 0 10px">Derived from a public xpub only -- this system cannot sign from these. See the Watcher &gt; Sweep page for the manual, operator-run BSC sweep tool.</p>
<div class="table-wrap">
<table>
<tr><th>Address</th><th>Role</th><th>Order</th><th>Status</th></tr>
{{ range .Addresses }}
<tr>
  <td class="mono">{{ .Address }}</td>
  <td><span class="pill-sm bsc">bsc_deposit_address</span></td>
  <td>{{ .OrderID }}</td>
  <td><span class="badge {{ if eq .Status "RETIRED" }}badge-neutral{{ else if eq .Status "FUNDED" }}badge-success{{ else }}badge-warning{{ end }}">{{ .Status }}</span></td>
</tr>
{{ else }}
<tr><td colspan="4"><div class="empty-state">No deposit addresses yet.</div></td></tr>
{{ end }}
</table>
</div>
`

type walletSlotRow struct {
	ID                           int
	TronAddress, Status, Balance string
}

type walletAddressRow struct {
	Address string
	OrderID int64
	Status  string
}

const sweepSlotContent = `
<div class="page-head"><h1>Sweep slot {{ .Slot.TronAddress }}</h1></div>
<p class="helptext">
  Sweeps this slot's live on-chain USDT balance, minus the reserve you
  choose, via S1's real signing flow -- exactly the same construction
  and signing path every real payout already uses. No private key is
  ever entered here or held by this console; S1 signs in-process, or
  the request drops into the existing 2-of-N approval queue if it's
  over threshold.
</p>
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}

<div class="card panel-narrow" style="margin-bottom:16px">
  <div class="fact"><span class="fact-label">status</span><span class="fact-value"><span class="badge {{ if eq .Slot.Status "ACTIVE" }}badge-success{{ else }}badge-neutral{{ end }}">{{ .Slot.Status }}</span></span></div>
  <div class="fact"><span class="fact-label">ledger balance</span><span class="fact-value mono">{{ .Slot.Balance }}</span></div>
</div>

{{ if .Result }}
<div class="card panel-narrow section">
  <h2 style="margin-bottom:10px">Result</h2>
  <div class="fact"><span class="fact-label">status</span><span class="fact-value"><span class="badge {{ if eq .Result.Status "SIGNED" }}badge-success{{ else }}badge-warning{{ end }}">{{ .Result.Status }}</span></span></div>
  <div class="fact"><span class="fact-label">swept_amount</span><span class="fact-value mono">{{ .Result.SweptAmount }}</span></div>
  {{ if .Result.TronTxID }}
  <div class="fact"><span class="fact-label">tron_txid</span><span class="fact-value mono">{{ .Result.TronTxID }}</span></div>
  <p class="field-hint" style="margin-top:10px">Track it at: https://tronscan.org/#/transaction/{{ .Result.TronTxID }}</p>
  {{ else }}
  <p class="field-hint" style="margin:10px 0">
    Over S1's auto-sign threshold -- awaiting a second approver in the
    <a href="/s1/approvals">S1 approval queue</a>. Once approved, finalize it here:
  </p>
  <form method="post" action="/wallets/sweep/finalize">
    <input type="hidden" name="slot_id" value="{{ .SlotID }}">
    <input type="hidden" name="signing_request_id" value="{{ .Result.SigningRequestID }}">
    <input type="hidden" name="unsigned_tx_hex" value="{{ .Result.UnsignedTxHex }}">
    <button class="btn btn-sm" type="submit">Check approval &amp; finalize</button>
  </form>
  {{ end }}
</div>
{{ else }}
<div class="card panel-narrow section">
  <h2 style="margin-bottom:10px">Sweep</h2>
  <form method="post" action="/wallets/{{ .SlotID }}/sweep">
    <label>destination_address</label>
    <input type="text" name="destination_address" placeholder="T..." required>
    <label>reserve_amount (decimal USDT left behind -- required, no default)</label>
    <input type="text" name="reserve_amount" placeholder="0.000000" required>
    <p style="margin-top:16px"><button class="btn btn-danger" type="submit" onclick="return confirm('This builds and requests a real signature for a real TRC20 transfer. Continue?');">Sweep</button></p>
  </form>
</div>
{{ end }}
`

type walletsPageData struct {
	basePageData
	Slots     []walletSlotRow
	Addresses []walletAddressRow
	Error     string
}

// getWallets lists every wallet this system actually knows about, by
// role -- see admin-panel-build-prompts.md's own OC.11. Two real
// sources, no invented third role: DispatcherClient.ListSlots() (C5's
// own registry, the only real "treasury wallet" list that exists --
// "Read this third" item 4) and WatcherClient.ListAddresses() (C2,
// receive-only by construction). Balance for slots comes from C5's own
// GET /v1/slots response directly -- already a real on-chain-backed
// field, not a second RPC client added here.
func (s *Server) getWallets(w http.ResponseWriter, r *http.Request) {
	data := walletsPageData{basePageData: s.newBasePageData(r)}

	slots, err := s.Dispatcher.ListSlots(r.Context())
	if err != nil {
		data.Error = "listing C5 slots: " + err.Error()
	}
	for _, sl := range slots {
		data.Slots = append(data.Slots, walletSlotRow{ID: sl.ID, TronAddress: sl.TronAddress, Status: sl.Status, Balance: sl.Balance})
	}

	addresses, err := s.Watcher.ListAddresses(r.Context(), 500)
	if err != nil {
		if data.Error != "" {
			data.Error += " / "
		}
		data.Error += "listing C2 addresses: " + err.Error()
	}
	for _, a := range addresses {
		data.Addresses = append(data.Addresses, walletAddressRow{Address: a.Address, OrderID: a.OrderID, Status: a.Status})
	}

	s.Templates.Render(w, "wallets", data)
}

type sweepSlotPageData struct {
	basePageData
	SlotID int
	Slot   walletSlotRow
	Result *opclient.SweepResult
	Error  string
}

func (s *Server) renderSweepSlot(w http.ResponseWriter, r *http.Request, id int, result *opclient.SweepResult, flashErr string) {
	data := sweepSlotPageData{basePageData: s.newBasePageData(r), SlotID: id, Result: result, Error: flashErr}
	slots, err := s.Dispatcher.ListSlots(r.Context())
	if err != nil {
		data.Error = "listing C5 slots: " + err.Error()
		s.Templates.Render(w, "sweep_slot", data)
		return
	}
	for _, sl := range slots {
		if sl.ID == id {
			data.Slot = walletSlotRow{ID: sl.ID, TronAddress: sl.TronAddress, Status: sl.Status, Balance: sl.Balance}
			break
		}
	}
	s.Templates.Render(w, "sweep_slot", data)
}

func (s *Server) getSweepSlot(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid slot id", http.StatusBadRequest)
		return
	}
	s.renderSweepSlot(w, r, id, nil, "")
}

// postSweepSlot builds and requests a real signature for a real TRC20
// transfer of this slot's live balance minus the operator-chosen
// reserve -- audit-logged before the call to C5 (invariant 3), same as
// every other write in this console.
func (s *Server) postSweepSlot(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid slot id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderSweepSlot(w, r, id, nil, "malformed form submission")
		return
	}
	dest := r.FormValue("destination_address")
	reserve := r.FormValue("reserve_amount")
	if dest == "" || reserve == "" {
		s.renderSweepSlot(w, r, id, nil, "destination_address and reserve_amount are both required")
		return
	}

	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "wallets.slot.sweep", "slot:"+strconv.Itoa(id), map[string]any{
		"destination_address": dest, "reserve_amount": reserve,
	})

	result, err := s.Dispatcher.SweepSlot(r.Context(), id, dest, reserve)
	if err != nil {
		s.renderSweepSlot(w, r, id, nil, "sweeping: "+err.Error())
		return
	}
	s.renderSweepSlot(w, r, id, &result, "")
}

// postSweepFinalize checks whether a PENDING sweep's signing request has
// since been approved and, if so, broadcasts it -- invariant 3 applies
// here too, logged before the call.
func (s *Server) postSweepFinalize(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form submission", http.StatusBadRequest)
		return
	}
	slotID, err := strconv.Atoi(r.FormValue("slot_id"))
	if err != nil {
		http.Error(w, "invalid slot id", http.StatusBadRequest)
		return
	}
	signingRequestID, err := strconv.ParseInt(r.FormValue("signing_request_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid signing_request_id", http.StatusBadRequest)
		return
	}
	unsignedTxHex := r.FormValue("unsigned_tx_hex")

	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "wallets.slot.sweep.finalize", "slot:"+strconv.Itoa(slotID),
		map[string]any{"signing_request_id": signingRequestID})

	result, err := s.Dispatcher.SweepFinalize(r.Context(), signingRequestID, unsignedTxHex)
	if err != nil {
		s.renderSweepSlot(w, r, slotID, nil, "finalizing: "+err.Error())
		return
	}
	s.renderSweepSlot(w, r, slotID, &result, "")
}
