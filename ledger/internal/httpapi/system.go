package httpapi

import (
	"context"
	"net/http"
	"sort"

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
	Halted           bool                  `json:"halted"`
	HaltReason       string                `json:"halt_reason,omitempty"`
	TrialBalance     map[string]string     `json:"trial_balance"`
	TrialBalanceOK   bool                  `json:"trial_balance_ok"`
	Discrepancies    []discrepancyDTO      `json:"discrepancies"`
	CacheOK          bool                  `json:"cache_ok"`
	CorridorPosition []corridorPositionDTO `json:"corridor_position"`
	ReconLagSeconds  float64               `json:"recon_lag_seconds"`
}

type discrepancyDTO struct {
	AccountCode   string `json:"account_code"`
	Asset         string `json:"asset"`
	CachedUnits   int64  `json:"cached_units"`
	ComputedUnits int64  `json:"computed_units"`
}

// corridorPositionDTO reports position:corridor for one asset against its
// configured ceiling (see internal/recon.Config.CorridorCeilings). An
// asset with no ceiling configured has nothing meaningful to compare
// against, so it's simply absent from CorridorPosition rather than shown
// with a zero ceiling that would read as "at capacity."
type corridorPositionDTO struct {
	Asset         string `json:"asset"`
	AccountCode   string `json:"account_code"`
	Position      string `json:"position"`
	PositionUnits int64  `json:"position_units"`
	CeilingUnits  int64  `json:"ceiling_units"`
	OverCeiling   bool   `json:"over_ceiling"`
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

	corridor, err := s.corridorPositions(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}

	var reconLag float64
	if s.Reconciler != nil {
		reconLag = s.Reconciler.LagSeconds()
	}

	respondJSON(w, http.StatusOK, invariantsResponse{
		Halted: halted, HaltReason: reason,
		TrialBalance: trialFormatted, TrialBalanceOK: trialOK,
		Discrepancies: dtos, CacheOK: len(dtos) == 0,
		CorridorPosition: corridor, ReconLagSeconds: reconLag,
	})
}

// corridorPositions reads position:corridor for every asset with a
// configured ceiling (see ReconCfg.CorridorCeilings), sorted by asset for
// a deterministic response. This mirrors internal/recon's own
// checkCorridorCeiling exactly -- same accounts, same magnitude
// comparison -- so this endpoint's "over ceiling" can never disagree with
// what actually triggers the reconciler's alert log line.
func (s *Server) corridorPositions(ctx context.Context) ([]corridorPositionDTO, error) {
	assets := make([]money.Asset, 0, len(s.ReconCfg.CorridorCeilings))
	for asset := range s.ReconCfg.CorridorCeilings {
		assets = append(assets, asset)
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i] < assets[j] })

	out := make([]corridorPositionDTO, 0, len(assets))
	for _, asset := range assets {
		ceiling := s.ReconCfg.CorridorCeilings[asset]
		code := "position:corridor:" + string(asset)
		bal, err := journal.Balance(ctx, s.Pool, code)
		if err != nil {
			return nil, err
		}
		formatted, err := money.Format(bal)
		if err != nil {
			return nil, err
		}
		magnitude := bal.Units
		if magnitude < 0 {
			magnitude = -magnitude
		}
		out = append(out, corridorPositionDTO{
			Asset: string(asset), AccountCode: code,
			Position: formatted, PositionUnits: bal.Units,
			CeilingUnits: ceiling, OverCeiling: magnitude > ceiling,
		})
	}
	return out, nil
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
