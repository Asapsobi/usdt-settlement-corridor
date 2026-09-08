package httpapi

import (
	"fmt"
	"net/http"
	"time"

	"dispatcher/internal/slots"
)

type slotResponse struct {
	ID             int        `json:"id"`
	TronAddress    string     `json:"tron_address"`
	Status         string     `json:"status"`
	Balance        string     `json:"balance"`
	TxCount        int64      `json:"tx_count"`
	ActivatedAt    time.Time  `json:"activated_at"`
	RetiredAt      *time.Time `json:"retired_at,omitempty"`
	LastDispatchAt *time.Time `json:"last_dispatch_at,omitempty"`
}

// getSlots is GET /v1/slots -- current status/balance/tx_count per slot,
// across every status (not just ACTIVE), so an operator can see a
// RETIRING slot's own remaining balance drain or a RETIRED slot's final
// resting balance too.
func (s *Server) getSlots(w http.ResponseWriter, r *http.Request) {
	var out []slotResponse
	for _, status := range []slots.Status{slots.StatusActive, slots.StatusRetiring, slots.StatusRetired} {
		list, err := s.Slots.List(r.Context(), status)
		if err != nil {
			writeErr(w, err)
			return
		}
		for _, slot := range list {
			balance, err := s.Ledger.GetAccountBalance(r.Context(), fmt.Sprintf("asset:tron:slot:%d", slot.ID))
			if err != nil {
				writeErr(w, err)
				return
			}
			out = append(out, slotResponse{
				ID: slot.ID, TronAddress: slot.TronAddress, Status: string(slot.Status),
				Balance: balance.Format(), TxCount: slot.TxCount,
				ActivatedAt: slot.ActivatedAt, RetiredAt: slot.RetiredAt, LastDispatchAt: slot.LastDispatchAt,
			})
		}
	}
	respondJSON(w, http.StatusOK, map[string]any{"slots": out})
}

type postRetireSlotRequest struct {
	// Immediate skips RETIRING and marks the slot RETIRED directly --
	// the same "not just RETIRING" path C5.9's own freeze handling uses,
	// exposed here for a manual override (e.g. an operator who already
	// knows a slot must never be used again for a reason DetectFreeze
	// itself didn't catch).
	Immediate bool `json:"immediate"`
}

// postRetireSlot is POST /v1/slots/{id}/retire -- a manual override of
// the normal cap-triggered rotation.
func (s *Server) postRetireSlot(w http.ResponseWriter, r *http.Request) {
	id, ok := urlParamInt(w, r, "id")
	if !ok {
		return
	}
	var req postRetireSlotRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	var err error
	if req.Immediate {
		err = s.Slots.MarkRetired(r.Context(), id)
	} else {
		err = s.Slots.MarkRetiring(r.Context(), id)
	}
	if err != nil {
		writeErr(w, err)
		return
	}

	slot, err := s.Slots.Get(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	balance, err := s.Ledger.GetAccountBalance(r.Context(), fmt.Sprintf("asset:tron:slot:%d", id))
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, slotResponse{
		ID: slot.ID, TronAddress: slot.TronAddress, Status: string(slot.Status),
		Balance: balance.Format(), TxCount: slot.TxCount,
		ActivatedAt: slot.ActivatedAt, RetiredAt: slot.RetiredAt, LastDispatchAt: slot.LastDispatchAt,
	})
}
