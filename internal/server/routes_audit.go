package server

import (
	"net/http"

	"github.com/ipedrazas/sbx/internal/audit"
)

func (s *Server) registerAudit(mux *http.ServeMux) {
	handle(mux, s.listAudit, "GET /manage/audit", "GET /manage/audit/")
}

// listAudit serves GET /manage/audit?agent_id=&dataset_id=&limit=&offset=,
// newest first; limit is clamped to 1..=500.
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	agentID, err := queryInt64(r, "agent_id")
	if err != nil {
		writeError(w, err)
		return
	}
	datasetID, err := queryInt64(r, "dataset_id")
	if err != nil {
		writeError(w, err)
		return
	}
	lim, err := queryInt64(r, "limit")
	if err != nil {
		writeError(w, err)
		return
	}
	off, err := queryInt64(r, "offset")
	if err != nil {
		writeError(w, err)
		return
	}
	limit := int64(50)
	if lim != nil {
		limit = min(max(*lim, 1), 500)
	}
	offset := int64(0)
	if off != nil {
		offset = max(*off, 0)
	}

	events, err := audit.ListEvents(r.Context(), s.db, agentID, datasetID, limit, offset)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	total, err := audit.CountEvents(r.Context(), s.db, agentID, datasetID)
	if err != nil {
		total = 0
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events": events,
		"meta":   map[string]any{"total": total, "limit": limit, "offset": offset},
	})
}
