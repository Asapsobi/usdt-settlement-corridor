package httpapi

import "net/http"

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

<h2 style="font-size:15px;margin:20px 0 10px">TRON payout slots (C5)</h2>
<p class="field-hint" style="margin:0 0 10px">
  Sweeping a slot via S1's own signing flow needs a new C5 route this
  build hasn't added yet (constructing the unsigned transaction) --
  not built here rather than faked. Tracked separately; see this
  page's own commit history.
</p>
<div class="table-wrap">
<table>
<tr><th>Address</th><th>Role</th><th>Status</th><th>Balance</th></tr>
{{ range .Slots }}
<tr>
  <td class="mono">{{ .TronAddress }}</td>
  <td><span class="pill-sm core">tron_slot</span></td>
  <td><span class="badge {{ if eq .Status "ACTIVE" }}badge-success{{ else if eq .Status "RETIRED" }}badge-neutral{{ else }}badge-warning{{ end }}">{{ .Status }}</span></td>
  <td class="mono">{{ .Balance }}</td>
</tr>
{{ else }}
<tr><td colspan="4"><div class="empty-state">No slots registered.</div></td></tr>
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
	TronAddress, Status, Balance string
}

type walletAddressRow struct {
	Address string
	OrderID int64
	Status  string
}

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
		data.Slots = append(data.Slots, walletSlotRow{TronAddress: sl.TronAddress, Status: sl.Status, Balance: sl.Balance})
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
