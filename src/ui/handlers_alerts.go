package ui

import (
	"net/http"
	"strings"
)

// handleAlerts surfaces the /api/alerts endpoint. The provider lives in
// Server.cfg.AlertsProvider — main.go wires either a Prometheus-backed
// provider (when PROMETHEUS_URL is set) or the local in-process evaluator.
//
// Response shape always includes a "source" hint per alert so the UI can
// label whether the user is looking at audit-grade Prometheus state or a
// best-effort local heuristic. We return 200 with an empty array when the
// provider yields no alerts; only a missing provider returns 503.
func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "GET required"})
		return
	}
	if s.cfg.AlertsProvider == nil {
		writeJSON(w, http.StatusServiceUnavailable, apiResponse{
			Message: "alerts not configured: set PROMETHEUS_URL to enable Prometheus integration, or rebuild with metrics registered for the local fallback",
		})
		return
	}

	severity := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("severity")))
	list, err := s.cfg.AlertsProvider.List(r.Context())
	if err != nil {
		s.cfg.Logger.Error(err, "list alerts")
		writeJSON(w, http.StatusBadGateway, apiResponse{Message: "alerts source unavailable: " + sanitizeError(err)})
		return
	}

	if severity != "" {
		filtered := list[:0]
		for _, a := range list {
			if strings.EqualFold(a.Severity, severity) {
				filtered = append(filtered, a)
			}
		}
		list = filtered
	}

	type response struct {
		AlertmanagerURL string         `json:"alertmanagerUrl,omitempty"`
		Counts          map[string]int `json:"counts"`
		Items           []any          `json:"items"`
	}
	resp := response{
		AlertmanagerURL: s.cfg.AlertmanagerURL,
		Counts:          map[string]int{"critical": 0, "warning": 0, "info": 0},
		Items:           make([]any, 0, len(list)),
	}
	for _, a := range list {
		resp.Counts[a.Severity]++
		resp.Items = append(resp.Items, a)
	}
	writeJSON(w, http.StatusOK, resp)
}
