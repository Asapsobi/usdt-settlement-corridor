package httpapi

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

const s1ApprovalsContent = `
<div class="page-head"><h1>S1 pending approvals</h1></div>
{{ if .Error }}<div class="flash flash-error">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
{{ if not .HasApproverToken }}
  <div class="flash flash-error">` + iconAlert + `<span>You logged in without an S1 approver token, so you can't approve or reject anything here -- that token is checked at login, never held by the console itself, so a decision always attributes to you, not to the console. Log out and back in with your own approver token to act.</span></div>
{{ end }}
<div class="table-wrap">
<table>
<tr><th>ID</th><th>Slot</th><th>Estimated USD</th><th>Created</th><th></th></tr>
{{ range .Requests }}
<tr>
  <td class="mono">{{ .ID }}</td><td>{{ .SlotID }}</td><td>${{ .EstimatedUSD }}</td><td class="mono">{{ .CreatedAt }}</td>
  <td>
    <div class="actions">
    <form class="inline" method="post" action="/s1/approvals/{{ .ID }}/approve">
      <button class="btn btn-sm btn-primary" type="submit" {{ if not $.HasApproverToken }}disabled title="log in with your own S1 approver token to approve"{{ end }}>Approve</button>
    </form>
    <form class="inline" method="post" action="/s1/approvals/{{ .ID }}/reject">
      <button class="btn btn-sm btn-danger" type="submit" {{ if not $.HasApproverToken }}disabled title="log in with your own S1 approver token to reject"{{ end }}>Reject</button>
    </form>
    </div>
  </td>
</tr>
{{ else }}
<tr><td colspan="5"><div class="empty-state">No pending approvals.</div></td></tr>
{{ end }}
</table>
</div>
`

type pendingApprovalRow struct {
	ID           int64
	SlotID       int
	EstimatedUSD float64
	CreatedAt    string
}

type s1ApprovalsPageData struct {
	basePageData
	Requests         []pendingApprovalRow
	HasApproverToken bool
	Error            string
}

func (s *Server) getS1Approvals(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFromContext(r.Context())
	data := s1ApprovalsPageData{basePageData: s.newBasePageData(r), HasApproverToken: sess.S1ApproverToken != ""}
	pending, err := s.S1.ListPendingApprovals(r.Context())
	if err != nil {
		data.Error = err.Error()
	}
	for _, p := range pending {
		data.Requests = append(data.Requests, pendingApprovalRow{ID: p.ID, SlotID: p.SlotID, EstimatedUSD: p.EstimatedUSD, CreatedAt: p.CreatedAt.String()})
	}
	s.Templates.Render(w, "s1_approvals", data)
}

// postS1Approve and postS1Reject are invariant 1 made concrete: neither
// ever touches a console-held credential. Without the operator's own
// approver token in their session, the action is refused outright --
// the disabled button in the template is a UI convenience, this check
// is the actual enforcement.
func (s *Server) postS1Approve(w http.ResponseWriter, r *http.Request) {
	s.decideApproval(w, r, true)
}

func (s *Server) postS1Reject(w http.ResponseWriter, r *http.Request) {
	s.decideApproval(w, r, false)
}

func (s *Server) decideApproval(w http.ResponseWriter, r *http.Request, approve bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid signing request id", http.StatusBadRequest)
		return
	}
	sess, _ := sessionFromContext(r.Context())
	if sess.S1ApproverToken == "" {
		http.Error(w, "opsconsole: no S1 approver token in your session -- log in again with one to approve or reject", http.StatusForbidden)
		return
	}

	action := "s1.approval.approve"
	if !approve {
		action = "s1.approval.reject"
	}
	_ = s.Audit.Write(sess.Username, action, "signing-request:"+strconv.FormatInt(id, 10), nil)

	if approve {
		_, err = s.S1.Approve(r.Context(), id, sess.S1ApproverToken)
	} else {
		_, err = s.S1.Reject(r.Context(), id, sess.S1ApproverToken)
	}
	if err != nil {
		http.Error(w, "opsconsole: "+err.Error(), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/s1/approvals", http.StatusFound)
}
