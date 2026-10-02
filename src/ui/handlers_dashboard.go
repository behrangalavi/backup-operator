package ui

import (
	"net/http"
	"strconv"
)

// --- Fleet heatmap ---

func (s *Server) handleAPIFleetHeatmap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "GET required")
		return
	}
	days := 30
	if q := r.URL.Query().Get("days"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			days = n
		}
	}
	summary, err := s.data.fleetHeatmap(r.Context(), days)
	if err != nil {
		s.cfg.Logger.Error(err, "fleet heatmap")
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, summary)
}
