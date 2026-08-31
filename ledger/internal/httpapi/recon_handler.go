package httpapi

import (
	"context"
	"math"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"ledger/internal/accounts"
	"ledger/internal/db"
	"ledger/internal/money"
	"ledger/internal/recon"
)

type postSnapshotRequest struct {
	AccountCode    string    `json:"account_code"`
	ObservedAmount string    `json:"observed_amount"`
	ChainRef       string    `json:"chain_ref"`
	Watermark      int64     `json:"watermark"`
	ObservedAt     time.Time `json:"observed_at"`
}

type snapshotResponse struct {
	DriftAmount string `json:"drift_amount"`
	Halted      bool   `json:"halted"`
}

// postSnapshot is POST /v1/reconciliation/snapshots. observed_amount is a
// decimal string, per the amounts-as-strings rule, but which asset it's
// denominated in is implied by the account (this endpoint doesn't accept
// an asset field at all) -- so the account is looked up first, purely to
// learn its asset, before the amount can even be parsed.
func (s *Server) postSnapshot(w http.ResponseWriter, r *http.Request) {
	var req postSnapshotRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	acc, err := accounts.GetByCode(r.Context(), s.Pool, req.AccountCode)
	if err != nil {
		writeErr(w, err)
		return
	}

	observed, err := money.ParseDecimal(req.ObservedAmount, acc.Asset)
	if err != nil {
		writeErr(w, err)
		return
	}

	var result recon.Result
	err = db.Tx(r.Context(), s.Pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = recon.IngestSnapshot(ctx, tx, s.ReconCfg, recon.SnapshotParams{
			AccountCode:   req.AccountCode,
			ObservedUnits: observed.Units,
			ChainRef:      req.ChainRef,
			Watermark:     req.Watermark,
			ObservedAt:    req.ObservedAt,
		})
		return err
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	s.Metrics.ReconDriftUnits.Observe(math.Abs(float64(result.DriftUnits)))

	driftAmount, err := money.Format(money.Amount{Asset: acc.Asset, Units: result.DriftUnits})
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, snapshotResponse{DriftAmount: driftAmount, Halted: result.Halted})
}
