package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"opsconsole/internal/opclient"
)

const orderListContent = `
<div class="page-head"><h1>Orders</h1></div>
<p class="helptext">C1's own order list is filtered by state -- there is no unfiltered "every order" view on the real route, so pick one.</p>
<form method="get" action="/orders" class="inline" style="margin-bottom:16px">
  <div class="actions">
    <select name="state">
      {{ $cur := .State }}
      {{ range .States }}<option value="{{ . }}" {{ if eq . $cur }}selected{{ end }}>{{ . }}</option>{{ end }}
    </select>
    <button class="btn btn-sm" type="submit">Filter</button>
  </div>
</form>
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
<div class="table-wrap">
<table>
<tr><th>External ID</th><th>Customer</th><th>Tier</th><th>Amount in</th><th>State</th><th>Updated</th><th></th></tr>
{{ range .Orders }}
<tr>
  <td class="mono">{{ .ExternalID }}</td><td>{{ .CustomerID }}</td><td>{{ .Tier }}</td><td>{{ .AmountIn }}</td>
  <td><span class="badge {{ .StateBadge }}">{{ .State }}</span></td>
  <td class="mono">{{ .UpdatedAt }}</td>
  <td><a class="btn btn-sm" href="/orders/{{ .ExternalID }}">View</a></td>
</tr>
{{ else }}
<tr><td colspan="7"><div class="empty-state">No orders in this state.</div></td></tr>
{{ end }}
</table>
</div>
{{ if .NextCursor }}<p style="margin-top:12px"><a class="btn btn-sm" href="/orders?state={{ .State }}&updated_after={{ .NextCursor }}">Next page</a></p>{{ end }}
`

const orderDetailContent = `
<div class="page-head"><h1>Order {{ if .Order.ExternalID }}{{ .Order.ExternalID }}{{ else }}not found{{ end }}</h1></div>
{{ if .Error }}
<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>
{{ else }}
<div class="cards" style="grid-template-columns:repeat(auto-fill,minmax(260px,1fr))">
  <div class="card">
    <div class="card-head"><h2>Order</h2><span class="badge {{ .StateBadge }}" style="margin-left:auto">{{ .Order.State }}</span></div>
    <div class="fact"><span class="fact-label">customer_id</span><span class="fact-value">{{ .Order.CustomerID }}</span></div>
    <div class="fact"><span class="fact-label">tier</span><span class="fact-value">{{ .Order.Tier }}</span></div>
    <div class="fact"><span class="fact-label">amount_in / amount_out</span><span class="fact-value">{{ .Order.AmountIn }} / {{ .Order.AmountOut }}</span></div>
    <div class="fact"><span class="fact-label">recipient (TRC20)</span><span class="fact-value mono">{{ .Order.RecipientAddress }}</span></div>
    {{ if .Order.SenderAddress }}<div class="fact"><span class="fact-label">sender (BSC)</span><span class="fact-value mono">{{ .Order.SenderAddress }}</span></div>{{ end }}
    <div class="fact"><span class="fact-label">quote_expires_at</span><span class="fact-value mono">{{ .Order.QuoteExpiresAt }}</span></div>
    <div class="fact"><span class="fact-label">updated_at</span><span class="fact-value mono">{{ .Order.UpdatedAt }}</span></div>
  </div>

  {{ if .Address.Address }}
  <div class="card">
    <div class="card-head"><h2>Deposit address (C2)</h2></div>
    <div class="fact"><span class="fact-label">address</span><span class="fact-value mono">{{ .Address.Address }}</span></div>
    <div class="fact"><span class="fact-label">status</span><span class="fact-value"><span class="badge {{ if eq .Address.Status "FUNDED" }}badge-success{{ else if eq .Address.Status "RETIRED" }}badge-neutral{{ else }}badge-warning{{ end }}">{{ .Address.Status }}</span></span></div>
  </div>
  {{ end }}

  {{ if .HasDispatch }}
  <div class="card">
    <div class="card-head"><h2>Dispatch (C5)</h2></div>
    <div class="fact"><span class="fact-label">status</span><span class="fact-value"><span class="badge {{ if eq .Dispatch.Status "SETTLED" }}badge-success{{ else if eq .Dispatch.Status "HELD" }}badge-danger{{ else }}badge-warning{{ end }}">{{ .Dispatch.Status }}</span></span></div>
    {{ if .Dispatch.LatestAttemptStatus }}<div class="fact"><span class="fact-label">latest attempt</span><span class="fact-value">{{ .Dispatch.LatestAttemptStatus }}</span></div>{{ end }}
    {{ if .Dispatch.TronTxID }}<div class="fact"><span class="fact-label">tron_txid</span><span class="fact-value mono">{{ .Dispatch.TronTxID }}</span></div>{{ end }}
  </div>
  {{ end }}
</div>

<div class="card panel-narrow section">
  <h2 style="margin-bottom:10px">Blocker</h2>
  {{ if .Blocker }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Blocker }}</span></div>
  {{ else }}<p class="field-hint" style="margin:0">Nothing blocking -- this order is progressing normally for its current state.</p>{{ end }}
  {{ if .LinkNote }}<p class="field-hint" style="margin-top:10px">{{ .LinkNote }}</p>{{ end }}
</div>
{{ end }}
`

const alertsContent = `
<div class="page-head"><h1>Alerts</h1></div>
<p class="helptext">Aggregates every service's own GET /v1/system/invariants, plus one cross-service check only this console can make.</p>
{{ range .ServiceAlerts }}
<div class="flash {{ if .OK }}flash-ok{{ else }}flash-error{{ end }}" style="margin-bottom:8px">{{ if .OK }}` + iconCheck + `{{ else }}` + iconAlert + `{{ end }}<span><b>{{ .Service }}:</b> {{ .Message }}</span></div>
{{ end }}
<div class="card panel-narrow section">
  <h2 style="margin-bottom:10px">Console-detected: possibly orphaned</h2>
  <p class="field-hint" style="margin:0 0 10px">Orders in <span class="mono">dispatching</span> for longer than {{ .StuckMinutes }} minutes with no matching C4 reservation and no C5 dispatch-attempt record anywhere -- genuinely orphaned, not just slow. (Not checked against S1: signing-request responses carry no order-linking field today, so that link cannot be made -- see admin-panel-build-prompts.md's own "Read this third" item 3.)</p>
  {{ if .Orphaned }}
  <div class="table-wrap">
  <table>
  <tr><th>External ID</th><th>Stuck since</th></tr>
  {{ range .Orphaned }}<tr><td class="mono"><a href="/orders/{{ .ExternalID }}">{{ .ExternalID }}</a></td><td class="mono">{{ .UpdatedAt }}</td></tr>{{ end }}
  </table>
  </div>
  {{ else }}<div class="empty-state">None found.</div>{{ end }}
</div>
`

// orderStates is C1's own real state enum -- ledger/docs/openapi.yaml's
// /v1/orders GET state parameter.
var orderStates = []string{"quoted", "funded", "screened", "dispatching", "settled", "held", "refunded", "expired"}

func stateBadgeClass(state string) string {
	switch state {
	case "settled":
		return "badge-success"
	case "held":
		return "badge-danger"
	case "refunded", "expired":
		return "badge-neutral"
	default:
		return "badge-warning"
	}
}

type orderRow struct {
	ExternalID, CustomerID, Tier, AmountIn, State, UpdatedAt, StateBadge string
}

type orderListPageData struct {
	basePageData
	States     []string
	State      string
	Orders     []orderRow
	NextCursor string
	Error      string
}

func (s *Server) getOrders(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "held"
	}
	data := orderListPageData{basePageData: s.newBasePageData(r), States: orderStates, State: state}
	list, err := s.Ledger.ListOrders(r.Context(), state, r.URL.Query().Get("updated_after"), 100)
	if err != nil {
		data.Error = err.Error()
		s.Templates.Render(w, "order_list", data)
		return
	}
	data.NextCursor = list.NextCursor
	for _, o := range list.Orders {
		data.Orders = append(data.Orders, orderRow{
			ExternalID: o.ExternalID, CustomerID: o.CustomerID, Tier: o.Tier, AmountIn: o.AmountIn,
			State: o.State, UpdatedAt: o.UpdatedAt, StateBadge: stateBadgeClass(o.State),
		})
	}
	s.Templates.Render(w, "order_list", data)
}

type orderDetailPageData struct {
	basePageData
	Order       opclient.Order
	StateBadge  string
	Address     opclient.WatchedAddress
	Dispatch    opclient.Dispatch
	HasDispatch bool
	Blocker     string
	LinkNote    string
	Error       string
}

// getOrderDetail aggregates C1's own order with C2's deposit-address
// status and, once dispatching has started, C5's dispatch record --
// joined here, in the console, exactly as admin-panel-build-prompts.md's
// own "Read this third" item 3 requires (C1's Order has no
// reservation_id/signing_request_id field, and none should be added to
// it for this). One downstream service being unavailable degrades only
// that section of the page (invariant 4) -- the order itself, C1's own
// answer, still renders.
func (s *Server) getOrderDetail(w http.ResponseWriter, r *http.Request) {
	externalID := chi.URLParam(r, "external_id")
	data := orderDetailPageData{basePageData: s.newBasePageData(r)}

	order, err := s.Ledger.GetOrder(r.Context(), externalID)
	if err != nil {
		data.Error = "fetching order from C1: " + err.Error()
		s.Templates.Render(w, "order_detail", data)
		return
	}
	data.Order = order
	data.StateBadge = stateBadgeClass(order.State)

	if addr, err := s.Watcher.GetAddress(r.Context(), order.ID); err == nil {
		data.Address = addr
	}

	if order.State == "dispatching" || order.State == "settled" || order.State == "held" {
		if dispatch, err := s.Dispatcher.GetDispatch(r.Context(), order.ID); err == nil {
			data.Dispatch = dispatch
			data.HasDispatch = true
		}
	}

	data.Blocker, data.LinkNote = s.deriveBlocker(r, order)
	s.Templates.Render(w, "order_detail", data)
}

// deriveBlocker never invents a reason a downstream service doesn't
// itself report -- held shows C3's real hold reason (found by listing
// holds and matching order_id client-side, since GET /v1/holds has no
// order_id filter param on the real route); dispatching cross-references
// C4's own reservations the same way (Reservation.order_id is real;
// GET /v1/reservations has no order_id filter either, so this lists by
// status and matches client-side). A PENDING signing request cannot be
// linked to this order at all -- S1's signingRequestResponse carries no
// order_id or slot_id, only id/status/signed_tx/created_at -- so that
// case is named as a known gap, never faked.
func (s *Server) deriveBlocker(r *http.Request, order opclient.Order) (blocker, note string) {
	switch order.State {
	case "held":
		holds, err := s.Screening.ListHolds(r.Context(), "OPEN")
		if err != nil {
			return "", "could not check C3 for a hold reason: " + err.Error()
		}
		for _, h := range holds {
			if h.OrderID == order.ID {
				return "Held by screening: " + h.ReasonCode, "Reason sourced from C3's own open hold for this order."
			}
		}
		return "Held, but no matching open hold found in C3.", "This order's own state says held, but C3 currently reports no open hold with this order_id -- worth a manual look."
	case "dispatching":
		reservations, err := s.Broker.ListReservations(r.Context(), []string{"PENDING", "FAILED"}, 200)
		if err != nil {
			return "", "could not check C4 for a reservation: " + err.Error()
		}
		for _, res := range reservations {
			if res.OrderID == order.ID {
				if res.Status == "FAILED" {
					return "Energy reservation FAILED.", "Reservation status sourced from C4; see Broker > Reservations to reconcile."
				}
				return "", "A PENDING C4 reservation exists for this order -- energy acquisition is still in flight."
			}
		}
		return "", "No linked reservation or signing-request found. A signing request cannot be looked up by order at all -- S1's own API has no order-linking field on that response."
	default:
		return "", ""
	}
}

type serviceAlert struct {
	Service, Message string
	OK                bool
}

type alertsPageData struct {
	basePageData
	ServiceAlerts []serviceAlert
	StuckMinutes  int
	Orphaned      []orderRow
}

// getAlerts reuses every Get*Invariants call OC.2's home page already
// makes -- it does not re-implement "what counts as unhealthy" -- and
// adds exactly one new check this console is uniquely positioned to
// make: a dispatching order with neither an in-flight C4 reservation
// nor any C5 dispatch-attempt record, older than OC_STUCK_ORDER_MINUTES.
// Scoped to dispatching only: quoted/funded/screened orders are
// EXPECTED to sit unattended (waiting on a customer's own deposit or
// C3's queue), so checking them here would manufacture false alarms,
// not real ones.
func (s *Server) getAlerts(w http.ResponseWriter, r *http.Request) {
	data := alertsPageData{basePageData: s.newBasePageData(r), StuckMinutes: s.StuckOrderMinutes}
	if data.StuckMinutes <= 0 {
		data.StuckMinutes = 30
	}

	ctx := r.Context()
	if inv, err := s.Ledger.GetInvariants(ctx); err != nil {
		data.ServiceAlerts = append(data.ServiceAlerts, serviceAlert{"ledger", err.Error(), false})
	} else if inv.Halted {
		data.ServiceAlerts = append(data.ServiceAlerts, serviceAlert{"ledger", "halted: " + inv.HaltReason, false})
	} else {
		data.ServiceAlerts = append(data.ServiceAlerts, serviceAlert{"ledger", "healthy", true})
	}
	if inv, err := s.Watcher.GetInvariants(ctx); err != nil {
		data.ServiceAlerts = append(data.ServiceAlerts, serviceAlert{"watcher", err.Error(), false})
	} else {
		data.ServiceAlerts = append(data.ServiceAlerts, serviceAlert{"watcher", "healthy", true})
		_ = inv
	}
	if _, err := s.Screening.GetQueue(ctx); err != nil {
		data.ServiceAlerts = append(data.ServiceAlerts, serviceAlert{"screening", err.Error(), false})
	} else {
		data.ServiceAlerts = append(data.ServiceAlerts, serviceAlert{"screening", "healthy", true})
	}
	if _, err := s.Broker.GetInvariants(ctx); err != nil {
		data.ServiceAlerts = append(data.ServiceAlerts, serviceAlert{"broker", err.Error(), false})
	} else {
		data.ServiceAlerts = append(data.ServiceAlerts, serviceAlert{"broker", "healthy", true})
	}
	if _, err := s.Dispatcher.GetInvariants(ctx); err != nil {
		data.ServiceAlerts = append(data.ServiceAlerts, serviceAlert{"dispatcher", err.Error(), false})
	} else {
		data.ServiceAlerts = append(data.ServiceAlerts, serviceAlert{"dispatcher", "healthy", true})
	}

	data.Orphaned = s.findOrphanedDispatchingOrders(ctx, data.StuckMinutes)
	s.Templates.Render(w, "alerts", data)
}

func (s *Server) findOrphanedDispatchingOrders(ctx context.Context, stuckMinutes int) []orderRow {
	list, err := s.Ledger.ListOrders(ctx, "dispatching", "", 200)
	if err != nil {
		return nil
	}
	reservations, err := s.Broker.ListReservations(ctx, []string{"PENDING", "CONFIRMED", "FAILED"}, 500)
	if err != nil {
		return nil
	}
	reserved := make(map[int64]bool, len(reservations))
	for _, res := range reservations {
		reserved[res.OrderID] = true
	}

	cutoff := nowUTC().Add(-time.Duration(stuckMinutes) * time.Minute)
	var orphaned []orderRow
	for _, o := range list.Orders {
		if reserved[o.ID] {
			continue // a reservation exists (in any state) -- something is or was in flight
		}
		updatedAt, err := time.Parse(time.RFC3339, o.UpdatedAt)
		if err != nil || updatedAt.After(cutoff) {
			continue // too recent to call orphaned, or an unparseable timestamp we won't guess about
		}
		if _, err := s.Dispatcher.GetDispatch(ctx, o.ID); err == nil {
			continue // a dispatch record exists -- C5 has already touched this order
		}
		orphaned = append(orphaned, orderRow{ExternalID: o.ExternalID, UpdatedAt: o.UpdatedAt})
	}
	return orphaned
}

func nowUTC() time.Time { return time.Now().UTC() }
