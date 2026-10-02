package ui

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// --- Manual backup trigger ---

func (s *Server) handleAPITriggerBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "POST required")
		return
	}
	targetName := trimPrefixPath(r.URL.Path, "/api/trigger/")
	if targetName == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "target name required")
		return
	}

	// Find the CronJob for this target.
	var cronJobs batchv1.CronJobList
	if err := s.cfg.Client.List(r.Context(), &cronJobs,
		client.InNamespace(s.cfg.Namespace),
		client.MatchingLabels{"app.kubernetes.io/managed-by": "backup-operator"},
	); err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "failed to list cronjobs")
		return
	}

	var cronJob *batchv1.CronJob
	for i := range cronJobs.Items {
		cj := &cronJobs.Items[i]
		if cj.Labels["backup.mogenius.io/target"] == targetName {
			cronJob = cj
			break
		}
	}
	if cronJob == nil {
		writeError(w, http.StatusNotFound, codeNotFound, "no cronjob found for target")
		return
	}

	jobName := fmt.Sprintf("manual-%s-%d", sanitizeName(targetName), time.Now().Unix())
	if len(jobName) > 52 {
		jobName = jobName[:52]
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: s.cfg.Namespace,
			Labels:    cronJob.Spec.JobTemplate.Labels,
		},
		Spec: cronJob.Spec.JobTemplate.Spec,
	}

	if err := s.cfg.Client.Create(r.Context(), job); err != nil {
		s.cfg.Logger.Error(err, "create manual job")
		writeError(w, http.StatusInternalServerError, codeInternal, "failed to create job")
		return
	}
	s.broadcast(sseEvent{Type: "backup_triggered", Data: targetName})
	writeJSON(w, http.StatusCreated, apiResponse{OK: true, Name: jobName, Message: "backup job created"})
}

// --- Job status ---

func (s *Server) handleAPIJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "GET required")
		return
	}

	var jobs batchv1.JobList
	if err := s.cfg.Client.List(r.Context(), &jobs, client.InNamespace(s.cfg.Namespace), client.MatchingLabels{
		"app.kubernetes.io/managed-by": "backup-operator",
	}); err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}

	type jobInfo struct {
		Name                     string  `json:"name"`
		Target                   string  `json:"target"`
		Status                   string  `json:"status"`
		StartTime                string  `json:"startTime,omitempty"`
		Duration                 string  `json:"duration,omitempty"`
		EstimatedDurationSeconds float64 `json:"estimatedDurationSeconds,omitempty"`
		EstimateSampleSize       int     `json:"estimateSampleSize,omitempty"`
	}

	out := make([]jobInfo, 0, len(jobs.Items))
	for _, j := range jobs.Items {
		status := "pending"
		if j.Status.Succeeded > 0 {
			status = "succeeded"
		} else if j.Status.Failed > 0 {
			status = "failed"
		} else if j.Status.Active > 0 {
			status = "running"
		}
		info := jobInfo{
			Name:   j.Name,
			Target: j.Labels["backup.mogenius.io/target"],
			Status: status,
		}
		if j.Status.StartTime != nil {
			info.StartTime = j.Status.StartTime.Format(time.RFC3339)
			if j.Status.CompletionTime != nil {
				info.Duration = j.Status.CompletionTime.Sub(j.Status.StartTime.Time).Round(time.Second).String()
			}
		}
		out = append(out, info)
	}

	// For running jobs, attach a duration estimate from past runs so the UI
	// can render a progress bar. Sequential (typical fleet has 0–3 running
	// jobs at once); the underlying meta lookups are 30s-cached.
	for i := range out {
		if out[i].Status != "running" || out[i].Target == "" {
			continue
		}
		dur, n, err := s.data.estimateDuration(r.Context(), out[i].Target, 10)
		if err != nil || n == 0 {
			continue
		}
		out[i].EstimatedDurationSeconds = dur.Seconds()
		out[i].EstimateSampleSize = n
	}

	if hasPaginationParams(r) {
		limit, offset := parsePagination(r, 50)
		total := len(out)
		if offset > total {
			offset = total
		}
		end := offset + limit
		if end > total {
			end = total
		}
		writeJSON(w, http.StatusOK, paginatedResponse{
			Items:  out[offset:end],
			Total:  total,
			Limit:  limit,
			Offset: offset,
		})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// SSE broker, subscribe, unsubscribe, publish, broadcast, handleSSE →
// see sse.go

// --- Helpers ---

func sanitizeName(name string) string {
	name = strings.ToLower(name)
	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, name)
	if len(name) > 40 {
		name = name[:40]
	}
	return strings.Trim(name, "-")
}

func sanitizeError(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "already exists") {
		return "resource already exists"
	}
	return "operation failed"
}
