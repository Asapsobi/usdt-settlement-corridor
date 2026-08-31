package httpapi

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"

	"ledger/internal/db"
	"ledger/internal/halt"
	"ledger/internal/journal"
	"ledger/internal/money"
)

type haltStateResponse struct {
	Halted bool   `json:"halted"`
	Reason string `json:"reason,omitempty"`
}

// getHalt is GET /v1/system/halt.
func (s *Server) getHalt(w http.ResponseWriter, r *http.Request) {
	halted, err := halt.IsHalted(r.Context(), s.Pool)
	if err != nil {
		writeErr(w, err)
		return
	}
	reason, err := halt.Reason(r.Context(), s.Pool)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, haltStateResponse{Halted: halted, Reason: reason})
}

type postHaltRequest struct {
	Action string         `json:"action"` // "set" or "clear"
	Reason string         `json:"reason,omitempty"`
	Detail map[string]any `json:"detail,omitempty"`
	Note   string         `json:"note,omitempty"`
}

// postHalt is POST /v1/system/halt, handling both set and clear. actor is
// always the authenticated caller (halted_by / cleared_by), never a
// request body field -- "the token maps to the actor recorded on every
// write" applies here exactly as everywhere else.
func (s *Server) postHalt(w http.ResponseWriter, r *http.Request) {
	var req postHaltRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	actor := actorFromContext(r.Context())

	var err error
	switch req.Action {
	case "set":
		err = db.Tx(r.Context(), s.Pool, func(ctx context.Context, tx pgx.Tx) error {
			return halt.Set(ctx, tx, halt.SetParams{Reason: req.Reason, Detail: req.Detail, Actor: actor})
		})
	case "clear":
		err = db.Tx(r.Context(), s.Pool, func(ctx context.Context, tx pgx.Tx) error {
			return halt.Clear(ctx, tx, halt.ClearParams{Actor: actor, Note: req.Note})
		})
	default:
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, `action must be "set" or "clear"`))
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}

	s.getHalt(w, r)
}

// invariantsResponse is, per the build spec, shaped so its numbers are
// the ones a customer's engineer would eventually see on the status page
// (S3) and diff against GET /v1/settlement-stats -- not built in this
// chunk, but this endpoint's shape is chosen with that in mind.
type invariantsResponse struct {
	Halted         bool              `json:"halted"`
	HaltReason     string            `json:"halt_reason,omitempty"`
	TrialBalance   map[string]string `json:"trial_balance"`
	TrialBalanceOK bool              `json:"trial_balance_ok"`
	Discrepancies  []discrepancyDTO  `json:"discrepancies"`
	CacheOK        bool              `json:"cache_ok"`
}

type discrepancyDTO struct {
	AccountCode   string `json:"account_code"`
	Asset         string `json:"asset"`
	CachedUnits   int64  `json:"cached_units"`
	ComputedUnits int64  `json:"computed_units"`
}

// getInvariants is GET /v1/system/invariants: the live results of every
// self-check the reconciler also runs on its own ticker, available
// on-demand rather than only every Config.Interval.
func (s *Server) getInvariants(w http.ResponseWriter, r *http.Request) {
	halted, err := halt.IsHalted(r.Context(), s.Pool)
	if err != nil {
		writeErr(w, err)
		return
	}
	reason, err := halt.Reason(r.Context(), s.Pool)
	if err != nil {
		writeErr(w, err)
		return
	}

	trial, err := journal.TrialBalance(r.Context(), s.Pool)
	if err != nil {
		writeErr(w, err)
		return
	}
	trialFormatted := make(map[string]string, len(trial))
	trialOK := true
	for asset, units := range trial {
		formatted, err := money.Format(money.Amount{Asset: asset, Units: units})
		if err != nil {
			writeErr(w, err)
			return
		}
		trialFormatted[string(asset)] = formatted
		if units != 0 {
			trialOK = false
		}
	}

	discrepancies, err := journal.VerifyBalances(r.Context(), s.Pool)
	if err != nil {
		writeErr(w, err)
		return
	}
	dtos := make([]discrepancyDTO, len(discrepancies))
	for i, d := range discrepancies {
		dtos[i] = discrepancyDTO{
			AccountCode: d.AccountCode, Asset: string(d.Asset),
			CachedUnits: d.CachedUnits, ComputedUnits: d.ComputedUnits,
		}
	}

	respondJSON(w, http.StatusOK, invariantsResponse{
		Halted: halted, HaltReason: reason,
		TrialBalance: trialFormatted, TrialBalanceOK: trialOK,
		Discrepancies: dtos, CacheOK: len(dtos) == 0,
	})
}

type healthzResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

func (s *Server) healthzHandler(w http.ResponseWriter, r *http.Request) {
	version, commit := "unknown", "unknown"
	if s.BuildInfo != nil {
		version, commit = s.BuildInfo()
	}
	respondJSON(w, http.StatusOK, healthzResponse{Status: "ok", Version: version, Commit: commit})
}

// readyzHandler additionally checks the database is actually reachable --
// healthz says the process is up, readyz says it can do its job.
func (s *Server) readyzHandler(w http.ResponseWriter, r *http.Request) {
	if err := s.Pool.Ping(r.Context()); err != nil {
		respondJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
