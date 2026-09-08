package httpapi

import (
	"fmt"
	"net/http"

	"dispatcher/internal/dispatch"
	"dispatcher/internal/slots"
)

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

// readyzHandler additionally checks the database is actually reachable
// -- healthz says the process is up, readyz says it can do its job.
func (s *Server) readyzHandler(w http.ResponseWriter, r *http.Request) {
	if err := s.Pool.Ping(r.Context()); err != nil {
		respondJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type slotHeadroomResponse struct {
	SlotID         int    `json:"slot_id"`
	Balance        string `json:"balance"`
	BalanceCeiling string `json:"balance_ceiling"`
	TxCount        int64  `json:"tx_count"`
	TxCountCeiling int64  `json:"tx_count_ceiling"`
}

type invariantsResponse struct {
	OpenDispatchingOrders      int                    `json:"open_dispatching_orders"`
	StuckPendingReconciliation int                    `json:"stuck_pending_reconciliation"`
	BatchQueueDepth            int                    `json:"batch_queue_depth"`
	SlotHeadroom               []slotHeadroomResponse `json:"slot_headroom"`
}

// getSystemInvariants is GET /v1/system/invariants -- open
// dispatching-without-held-or-settled orders (the C5.7 reconciliation
// job's own findings, surfaced for an operator, not just acted on
// silently on a ticker), batch queue depth, and slot cap headroom.
func (s *Server) getSystemInvariants(w http.ResponseWriter, r *http.Request) {
	dispatching, err := s.Dispatcher.Store.ListDispatching(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}

	stuck := 0
	for _, local := range dispatching {
		latest, err := s.Dispatcher.Attempts.LatestForOrder(r.Context(), local.OrderID)
		if err != nil {
			continue // no attempt yet at all -- legitimately still in flight, not stuck
		}
		if latest.Status == dispatch.BroadcastFailed {
			stuck++
		}
	}

	batchQueueDepth := 0
	if s.Dispatcher.Batches != nil {
		queued, err := s.Dispatcher.Batches.ListQueued(r.Context(), 100000)
		if err != nil {
			writeErr(w, err)
			return
		}
		batchQueueDepth = len(queued)
	}

	var headroom []slotHeadroomResponse
	active, err := s.Slots.List(r.Context(), slots.StatusActive)
	if err != nil {
		writeErr(w, err)
		return
	}
	for _, slot := range active {
		balance, err := s.Ledger.GetAccountBalance(r.Context(), fmt.Sprintf("asset:tron:slot:%d", slot.ID))
		if err != nil {
			writeErr(w, err)
			return
		}
		headroom = append(headroom, slotHeadroomResponse{
			SlotID: slot.ID, Balance: balance.Format(), BalanceCeiling: s.SlotCaps.BalanceCeiling.Format(),
			TxCount: slot.TxCount, TxCountCeiling: s.SlotCaps.TxCountCeiling,
		})
	}

	respondJSON(w, http.StatusOK, invariantsResponse{
		OpenDispatchingOrders: len(dispatching), StuckPendingReconciliation: stuck,
		BatchQueueDepth: batchQueueDepth, SlotHeadroom: headroom,
	})
}
