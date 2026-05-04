package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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

// handleAlertsStatus reports connectivity to Prometheus and Alertmanager.
// GET /api/alerts/status
func (s *Server) handleAlertsStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "GET required"})
		return
	}

	type componentStatus struct {
		Configured bool   `json:"configured"`
		URL        string `json:"url,omitempty"`
		Reachable  bool   `json:"reachable"`
		Error      string `json:"error,omitempty"`
		Version    string `json:"version,omitempty"`
	}

	type statusResponse struct {
		Prometheus   componentStatus `json:"prometheus"`
		Alertmanager componentStatus `json:"alertmanager"`
		LocalAlerts  bool            `json:"localAlerts"`
		Mode         string          `json:"mode"` // "prometheus", "local", "none"
	}

	resp := statusResponse{LocalAlerts: s.cfg.AlertsProvider != nil}

	client := &http.Client{Timeout: 5 * time.Second}

	// Check Prometheus
	if s.cfg.PrometheusURL != "" {
		resp.Prometheus.Configured = true
		resp.Prometheus.URL = s.cfg.PrometheusURL
		endpoint := strings.TrimRight(s.cfg.PrometheusURL, "/") + "/api/v1/status/buildinfo"
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, endpoint, nil)
		if req != nil {
			if httpResp, err := client.Do(req); err != nil {
				resp.Prometheus.Error = "unreachable"
			} else {
				defer func() { _ = httpResp.Body.Close() }()
				if httpResp.StatusCode == http.StatusOK {
					resp.Prometheus.Reachable = true
					var body struct {
						Data struct {
							Version string `json:"version"`
						} `json:"data"`
					}
					if err := json.NewDecoder(httpResp.Body).Decode(&body); err == nil {
						resp.Prometheus.Version = body.Data.Version
					}
				} else {
					resp.Prometheus.Error = fmt.Sprintf("HTTP %d", httpResp.StatusCode)
				}
			}
		}
		resp.Mode = "prometheus"
		if !resp.Prometheus.Reachable {
			resp.Mode = "local" // fallback
		}
	} else {
		resp.Mode = "local"
	}

	// Check Alertmanager
	if s.cfg.AlertmanagerURL != "" {
		resp.Alertmanager.Configured = true
		resp.Alertmanager.URL = s.cfg.AlertmanagerURL
		endpoint := strings.TrimRight(s.cfg.AlertmanagerURL, "/") + "/api/v2/status"
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, endpoint, nil)
		if req != nil {
			if httpResp, err := client.Do(req); err != nil {
				resp.Alertmanager.Error = "unreachable"
			} else {
				defer func() { _ = httpResp.Body.Close() }()
				if httpResp.StatusCode == http.StatusOK {
					resp.Alertmanager.Reachable = true
					var body struct {
						VersionInfo struct {
							Version string `json:"version"`
						} `json:"versionInfo"`
					}
					if err := json.NewDecoder(httpResp.Body).Decode(&body); err == nil {
						resp.Alertmanager.Version = body.VersionInfo.Version
					}
				} else {
					resp.Alertmanager.Error = fmt.Sprintf("HTTP %d", httpResp.StatusCode)
				}
			}
		}
	}

	if !resp.Prometheus.Configured && !resp.Alertmanager.Configured && !resp.LocalAlerts {
		resp.Mode = "none"
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleAlertsTest sends a test alert to Alertmanager.
// POST /api/alerts/test
func (s *Server) handleAlertsTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "POST required"})
		return
	}
	if s.cfg.ReadOnly {
		writeJSON(w, http.StatusForbidden, apiResponse{Message: "read-only mode"})
		return
	}
	if s.cfg.AlertmanagerURL == "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{
			Message: "Alertmanager not configured. Set alerts.alertmanagerURL in Helm values.",
		})
		return
	}

	// Build a test alert in Alertmanager v2 format.
	now := time.Now().UTC()
	testAlerts := []map[string]any{{
		"labels": map[string]string{
			"alertname": "BackupOperatorTestAlert",
			"severity":  "info",
			"target":    "test-target",
			"source":    "backup-operator-ui",
		},
		"annotations": map[string]string{
			"summary":     "Test alert from Backup Operator UI",
			"description": "This is a test alert to verify the Prometheus → Alertmanager → Notification pipeline is working correctly. This alert will auto-resolve.",
		},
		"startsAt": now.Format(time.RFC3339),
		"endsAt":   now.Add(2 * time.Minute).Format(time.RFC3339),
	}}

	body, _ := json.Marshal(testAlerts)
	endpoint := strings.TrimRight(s.cfg.AlertmanagerURL, "/") + "/api/v2/alerts"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "failed to build request"})
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		s.cfg.Logger.Error(err, "test alert: alertmanager unreachable")
		writeJSON(w, http.StatusBadGateway, apiResponse{
			Message: "Alertmanager unreachable: " + err.Error(),
		})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		writeJSON(w, http.StatusOK, apiResponse{
			OK:      true,
			Message: "Test alert sent to Alertmanager. It will auto-resolve in 2 minutes. Check your notification channel (Slack, Email, etc.).",
		})
		return
	}

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	s.cfg.Logger.Error(fmt.Errorf("alertmanager returned %d: %s", resp.StatusCode, string(respBody)), "test alert failed")
	writeJSON(w, http.StatusBadGateway, apiResponse{
		Message: fmt.Sprintf("Alertmanager returned HTTP %d", resp.StatusCode),
	})
}
