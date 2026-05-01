package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// settingsPayload is the JSON shape for GET and PUT /api/settings.
type settingsPayload struct {
	DefaultSchedule     string `json:"defaultSchedule"`
	RunTimeoutSeconds   string `json:"runTimeoutSeconds"`
	DefaultRetentionDays string `json:"defaultRetentionDays"`
	DefaultMinKeep      string `json:"defaultMinKeep"`
	TempDir             string `json:"tempDir"`
	TempDirSize         string `json:"tempDirSize"`
	WorkerCPULimit      string `json:"workerCpuLimit"`
	WorkerMemoryLimit   string `json:"workerMemoryLimit"`
	WorkerCPURequest    string `json:"workerCpuRequest"`
	WorkerMemoryRequest string `json:"workerMemoryRequest"`
}

// settingsResponse wraps a settingsPayload with the standard API shape.
type settingsResponse struct {
	OK       bool            `json:"ok"`
	Message  string          `json:"message,omitempty"`
	Settings *settingsPayload `json:"settings,omitempty"`
}

// handleGetSettings reads the settings ConfigMap and returns the current values.
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SettingsConfigMap == "" {
		writeJSON(w, http.StatusNotFound, settingsResponse{Message: "settings not configured"})
		return
	}

	cm := &corev1.ConfigMap{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{
		Namespace: s.cfg.Namespace,
		Name:      s.cfg.SettingsConfigMap,
	}, cm); err != nil {
		s.cfg.Logger.Error(err, "get settings configmap")
		writeJSON(w, http.StatusInternalServerError, settingsResponse{Message: "failed to read settings"})
		return
	}

	writeJSON(w, http.StatusOK, settingsResponse{
		OK: true,
		Settings: &settingsPayload{
			DefaultSchedule:      cm.Data["defaultSchedule"],
			RunTimeoutSeconds:    cm.Data["runTimeoutSeconds"],
			DefaultRetentionDays: cm.Data["defaultRetentionDays"],
			DefaultMinKeep:       cm.Data["defaultMinKeep"],
			TempDir:              cm.Data["tempDir"],
			TempDirSize:          cm.Data["tempDirSize"],
			WorkerCPULimit:       cm.Data["workerCpuLimit"],
			WorkerMemoryLimit:    cm.Data["workerMemoryLimit"],
			WorkerCPURequest:     cm.Data["workerCpuRequest"],
			WorkerMemoryRequest:  cm.Data["workerMemoryRequest"],
		},
	})
}

// handleUpdateSettings validates and writes new settings to the ConfigMap.
func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SettingsConfigMap == "" {
		writeJSON(w, http.StatusNotFound, settingsResponse{Message: "settings not configured"})
		return
	}

	var req settingsPayload
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.cfg.Logger.Error(err, "decode settings JSON")
		writeJSON(w, http.StatusBadRequest, settingsResponse{Message: "invalid JSON"})
		return
	}

	if err := validateSettings(req); err != nil {
		writeJSON(w, http.StatusBadRequest, settingsResponse{Message: err.Error()})
		return
	}

	cm := &corev1.ConfigMap{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{
		Namespace: s.cfg.Namespace,
		Name:      s.cfg.SettingsConfigMap,
	}, cm); err != nil {
		s.cfg.Logger.Error(err, "get settings configmap for update")
		writeJSON(w, http.StatusInternalServerError, settingsResponse{Message: "failed to read settings"})
		return
	}

	// Verify this is the settings ConfigMap (has the component label).
	if cm.Labels["app.kubernetes.io/component"] != "settings" {
		writeJSON(w, http.StatusForbidden, settingsResponse{Message: "not a settings configmap"})
		return
	}

	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data["defaultSchedule"] = req.DefaultSchedule
	cm.Data["runTimeoutSeconds"] = req.RunTimeoutSeconds
	cm.Data["defaultRetentionDays"] = req.DefaultRetentionDays
	cm.Data["defaultMinKeep"] = req.DefaultMinKeep
	cm.Data["tempDir"] = req.TempDir
	cm.Data["tempDirSize"] = req.TempDirSize
	cm.Data["workerCpuLimit"] = req.WorkerCPULimit
	cm.Data["workerMemoryLimit"] = req.WorkerMemoryLimit
	cm.Data["workerCpuRequest"] = req.WorkerCPURequest
	cm.Data["workerMemoryRequest"] = req.WorkerMemoryRequest

	if err := s.cfg.Client.Update(r.Context(), cm); err != nil {
		s.cfg.Logger.Error(err, "update settings configmap")
		writeJSON(w, http.StatusInternalServerError, settingsResponse{Message: "failed to save settings"})
		return
	}

	s.broadcast(sseEvent{Type: "settings_updated", Data: "settings"})
	writeJSON(w, http.StatusOK, settingsResponse{OK: true, Message: "settings saved"})
}

// handleSettingsExport generates a values.yaml snippet from the current settings.
func (s *Server) handleSettingsExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "GET required"})
		return
	}
	if s.cfg.SettingsConfigMap == "" {
		writeJSON(w, http.StatusNotFound, settingsResponse{Message: "settings not configured"})
		return
	}

	cm := &corev1.ConfigMap{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{
		Namespace: s.cfg.Namespace,
		Name:      s.cfg.SettingsConfigMap,
	}, cm); err != nil {
		s.cfg.Logger.Error(err, "get settings configmap for export")
		writeJSON(w, http.StatusInternalServerError, settingsResponse{Message: "failed to read settings"})
		return
	}

	yaml := buildValuesYAML(cm.Data)

	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="values.yaml"`)
	_, _ = w.Write([]byte(yaml))
}

func validateSettings(s settingsPayload) error {
	if s.DefaultSchedule == "" {
		return fmt.Errorf("defaultSchedule is required")
	}
	if s.RunTimeoutSeconds != "" {
		if n, err := strconv.Atoi(s.RunTimeoutSeconds); err != nil || n < 0 {
			return fmt.Errorf("runTimeoutSeconds must be a non-negative integer")
		}
	}
	if s.DefaultRetentionDays != "" {
		if n, err := strconv.Atoi(s.DefaultRetentionDays); err != nil || n < 0 {
			return fmt.Errorf("defaultRetentionDays must be a non-negative integer")
		}
	}
	if s.DefaultMinKeep != "" {
		if n, err := strconv.Atoi(s.DefaultMinKeep); err != nil || n < 0 {
			return fmt.Errorf("defaultMinKeep must be a non-negative integer")
		}
	}
	return nil
}

func buildValuesYAML(data map[string]string) string {
	var b strings.Builder
	b.WriteString("# Generated by backup-operator Settings Wizard\n")
	b.WriteString("# Apply with: helm upgrade backup-operator ./charts/backup-operator -f values.yaml\n\n")

	b.WriteString("config:\n")
	writeYAMLField(&b, "  ", "defaultSchedule", data["defaultSchedule"])
	writeYAMLField(&b, "  ", "runTimeoutSeconds", data["runTimeoutSeconds"])
	writeYAMLField(&b, "  ", "defaultRetentionDays", data["defaultRetentionDays"])
	writeYAMLField(&b, "  ", "defaultMinKeep", data["defaultMinKeep"])
	writeYAMLField(&b, "  ", "tempDir", data["tempDir"])

	b.WriteString("\ntempDirSize: ")
	b.WriteString(quote(data["tempDirSize"]))
	b.WriteString("\n")

	b.WriteString("\nworkerResources:\n")
	b.WriteString("  limits:\n")
	writeYAMLField(&b, "    ", "cpu", data["workerCpuLimit"])
	writeYAMLField(&b, "    ", "memory", data["workerMemoryLimit"])
	b.WriteString("  requests:\n")
	writeYAMLField(&b, "    ", "cpu", data["workerCpuRequest"])
	writeYAMLField(&b, "    ", "memory", data["workerMemoryRequest"])

	b.WriteString("\nui:\n  enabled: true\n")

	return b.String()
}

func writeYAMLField(b *strings.Builder, indent, key, value string) {
	b.WriteString(indent)
	b.WriteString(key)
	b.WriteString(": ")
	if _, err := strconv.Atoi(value); err == nil {
		b.WriteString(value)
	} else {
		b.WriteString(quote(value))
	}
	b.WriteString("\n")
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
