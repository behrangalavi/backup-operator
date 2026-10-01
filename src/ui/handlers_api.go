package ui

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"backup-operator/api/v1alpha1"
	"backup-operator/internal/labels"
	"backup-operator/internal/meta"
	"backup-operator/internal/safe"
	"backup-operator/internal/secrets"
	"backup-operator/metrics"

	dto "github.com/prometheus/client_model/go"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type apiResponse struct {
	OK bool `json:"ok"`
	// Code is a stable machine-readable error tag — see errors_api.go for
	// the catalogue. Frontend uses it to pick toast severity and (later)
	// localised messages without parsing Message text.
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Name    string `json:"name,omitempty"`
}

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

// --- Destination storage stats ---

type destStorageStats struct {
	Name           string `json:"name"`
	StorageType    string `json:"storageType"`
	TotalFiles     int    `json:"totalFiles"`
	TotalSizeBytes int64  `json:"totalSizeBytes"`
	BackupCount    int    `json:"backupCount"`
	MetaCount      int    `json:"metaCount"`
	OldestBackup   string `json:"oldestBackup,omitempty"`
	NewestBackup   string `json:"newestBackup,omitempty"`
	Error          string `json:"error,omitempty"`
}

func (s *Server) handleAPIDestinationStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "GET required")
		return
	}

	dests, err := s.data.listDestinations(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}

	results := make([]destStorageStats, len(dests))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range dests {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			dest := dests[idx]
			defer safe.Goroutine(s.cfg.Logger, "destination-stats", dest.Name)
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-r.Context().Done():
				return
			}

			st, err := s.storageFor(dest, "stats")
			if err != nil {
				results[idx] = destStorageStats{
					Name:        dest.Name,
					StorageType: dest.StorageType,
					Error:       "storage init failed",
				}
				return
			}

			// Short timeout: this is a live UI probe, not the backup run.
			// A broken destination must surface fast or the dashboard
			// blocks behind it on every page-load and SSE refresh.
			ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
			defer cancel()

			objs, err := st.List(ctx, "")
			if err != nil {
				results[idx] = destStorageStats{
					Name:        dest.Name,
					StorageType: dest.StorageType,
					Error:       sanitizeStorageError(err),
				}
				return
			}

			stat := destStorageStats{
				Name:        dest.Name,
				StorageType: dest.StorageType,
				TotalFiles:  len(objs),
			}
			for _, o := range objs {
				stat.TotalSizeBytes += o.Size
				if strings.HasSuffix(o.Path, ".meta.json") {
					stat.MetaCount++
				} else if labels.IsDumpSuffix(o.Path) {
					stat.BackupCount++
				}
			}
			if stat.MetaCount > 0 {
				var oldest, newest string
				for _, o := range objs {
					if !strings.HasSuffix(o.Path, ".meta.json") {
						continue
					}
					ts := extractTimestamp(o.Path)
					if ts == "" {
						continue
					}
					if oldest == "" || ts < oldest {
						oldest = ts
					}
					if newest == "" || ts > newest {
						newest = ts
					}
				}
				stat.OldestBackup = oldest
				stat.NewestBackup = newest
			}
			results[idx] = stat
		}(i)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, results)
}

// extractTimestamp pulls a compact timestamp from a meta path like
// "target/2026/05/01/dump-20260501T020000Z.meta.json" → "20260501T020000Z"
func extractTimestamp(p string) string {
	base := p
	if idx := strings.LastIndex(p, "/"); idx >= 0 {
		base = p[idx+1:]
	}
	base = strings.TrimPrefix(base, "dump-")
	base = strings.TrimSuffix(base, ".meta.json")
	if len(base) == 16 && base[8] == 'T' && base[15] == 'Z' {
		return base
	}
	return ""
}

// --- Destination health matrix ---

type destHealthEntry struct {
	Target      string `json:"target"`
	Destination string `json:"destination"`
	StorageType string `json:"storageType"`
	HasBackup   bool   `json:"hasBackup"`
	LatestRun   string `json:"latestRun,omitempty"`
	Status      string `json:"status"` // "ok", "failed", "missing", "unreachable"
	Error       string `json:"error,omitempty"`

	// Scrub fields are populated when STORAGE_SCRUB_ENABLED=true and at least
	// one scrub has run for this pair. ScrubStatus is "" when no scrub data
	// is available — the UI can then either hide the column or render
	// "disabled" depending on whether the operator advertises scrub support.
	ScrubStatus      string `json:"scrubStatus,omitempty"`      // "ok" | "failed"
	ScrubLastCheck   int64  `json:"scrubLastCheck,omitempty"`   // unix seconds
	ScrubFailedTotal int64  `json:"scrubFailedTotal,omitempty"` // cumulative failures
}

func (s *Server) handleAPIDestinationHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "GET required")
		return
	}

	sources, err := s.data.listTargets(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}

	dests, err := s.data.listDestinations(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}

	type lookupResult struct {
		latest map[string]*meta.MetaFile
		err    error
	}
	destLatest := make(map[string]lookupResult, len(dests))
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, 4)
	for _, dest := range dests {
		wg.Add(1)
		go func(d *secrets.Destination) {
			defer wg.Done()
			defer safe.Goroutine(s.cfg.Logger, "destination-health", d.Name)
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-r.Context().Done():
				return
			}

			st, err := s.storageFor(d, "health")
			if err != nil {
				mu.Lock()
				destLatest[d.Name] = lookupResult{err: err}
				mu.Unlock()
				return
			}
			// Short timeout — see destination-stats handler for rationale.
			ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
			defer cancel()
			latest, err := meta.LatestPerTarget(ctx, st)
			mu.Lock()
			destLatest[d.Name] = lookupResult{latest: latest, err: err}
			mu.Unlock()
		}(dest)
	}
	wg.Wait()

	var entries []destHealthEntry
	for _, src := range sources {
		allowedDests := src.Destinations
		for _, dest := range dests {
			isAllowed := len(allowedDests) == 0
			if !isAllowed {
				for _, ad := range allowedDests {
					if ad == dest.Name {
						isAllowed = true
						break
					}
				}
			}
			if !isAllowed {
				continue
			}

			entry := destHealthEntry{
				Target:      src.Name,
				Destination: dest.Name,
				StorageType: dest.StorageType,
			}

			lr, ok := destLatest[dest.Name]
			if !ok || lr.err != nil {
				entry.Status = "unreachable"
				if lr.err != nil {
					entry.Error = sanitizeStorageError(lr.err)
				}
				entries = append(entries, entry)
				continue
			}

			m, exists := lr.latest[src.Name]
			if !exists {
				entry.Status = "missing"
				entries = append(entries, entry)
				continue
			}

			entry.HasBackup = true
			entry.LatestRun = m.Timestamp
			if m.IsFailure() {
				entry.Status = "failed"
				entry.Error = m.Error
			} else {
				entry.Status = "ok"
			}
			entries = append(entries, entry)
		}
	}

	// Decorate with scrub status from our own metric registry. Done last so
	// it's a no-op when STORAGE_SCRUB_ENABLED is off (the gauges are simply
	// absent and the lookup returns zero values).
	scrub := collectScrubStatus()
	for i := range entries {
		key := entries[i].Target + "\x00" + entries[i].Destination
		if s, ok := scrub[key]; ok {
			entries[i].ScrubStatus = s.status
			entries[i].ScrubLastCheck = s.lastCheck
			entries[i].ScrubFailedTotal = s.failedTotal
		}
	}
	writeJSON(w, http.StatusOK, entries)
}

type scrubInfo struct {
	status      string // "ok" | "failed"
	lastCheck   int64
	failedTotal int64
}

// collectScrubStatus walks the operator's own metric registry and indexes
// scrub gauges by (target, destination). Used to decorate destination-health
// entries without an extra storage round-trip — the scrub already touched
// storage on its own schedule, so there is no point re-doing it per UI hit.
func collectScrubStatus() map[string]scrubInfo {
	out := map[string]scrubInfo{}
	g := metrics.Gatherer()
	if g == nil {
		return out
	}
	families, err := g.Gather()
	if err != nil {
		return out
	}
	getOrInit := func(target, dest string) scrubInfo {
		k := target + "\x00" + dest
		return out[k]
	}
	put := func(target, dest string, info scrubInfo) {
		out[target+"\x00"+dest] = info
	}
	for _, fam := range families {
		switch fam.GetName() {
		case metrics.NameStorageScrubPassed:
			for _, m := range fam.Metric {
				target, dest := scrubLabels(m.Label)
				if target == "" {
					continue
				}
				info := getOrInit(target, dest)
				if m.Gauge != nil && m.Gauge.GetValue() == 1 {
					info.status = "ok"
				} else {
					info.status = "failed"
				}
				put(target, dest, info)
			}
		case metrics.NameStorageScrubLastCheckTimestampSeconds:
			for _, m := range fam.Metric {
				target, dest := scrubLabels(m.Label)
				if target == "" {
					continue
				}
				info := getOrInit(target, dest)
				if m.Gauge != nil {
					info.lastCheck = int64(m.Gauge.GetValue())
				}
				put(target, dest, info)
			}
		case metrics.NameStorageScrubFailedTotal:
			for _, m := range fam.Metric {
				target, dest := scrubLabels(m.Label)
				if target == "" {
					continue
				}
				info := getOrInit(target, dest)
				if m.Counter != nil {
					info.failedTotal = int64(m.Counter.GetValue())
				}
				put(target, dest, info)
			}
		}
	}
	return out
}

func scrubLabels(pairs []*dto.LabelPair) (target, dest string) {
	for _, p := range pairs {
		switch p.GetName() {
		case "target":
			target = p.GetValue()
		case "destination":
			dest = p.GetValue()
		}
	}
	return
}

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

// --- Backup consistency check ---

type consistencyIssue struct {
	Target      string   `json:"target"`
	Timestamp   string   `json:"timestamp"`
	PresentIn   []string `json:"presentIn"`
	MissingFrom []string `json:"missingFrom"`
}

func (s *Server) handleAPIConsistencyCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "GET required")
		return
	}

	sources, err := s.data.listTargets(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}

	dests, err := s.data.listDestinations(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}

	// Fetch runs per destination
	type destRuns struct {
		name       string
		timestamps map[string]bool
	}
	allDestRuns := make([]destRuns, len(dests))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, dest := range dests {
		allDestRuns[i].name = dest.Name
		allDestRuns[i].timestamps = make(map[string]bool)
		wg.Add(1)
		go func(idx int, d *secrets.Destination) {
			defer wg.Done()
			defer safe.Goroutine(s.cfg.Logger, "consistency-check", d.Name)
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-r.Context().Done():
				return
			}

			st, err := s.storageFor(d, "consistency")
			if err != nil {
				return
			}
			// Short timeout — see destination-stats handler for rationale.
			ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
			defer cancel()
			objs, err := st.List(ctx, "")
			if err != nil {
				return
			}
			for _, o := range objs {
				if strings.HasSuffix(o.Path, ".meta.json") {
					parts := strings.SplitN(o.Path, "/", 2)
					if len(parts) >= 1 {
						ts := extractTimestamp(o.Path)
						if ts != "" {
							allDestRuns[idx].timestamps[parts[0]+"@"+ts] = true
						}
					}
				}
			}
		}(i, dest)
	}
	wg.Wait()

	var issues []consistencyIssue
	for _, src := range sources {
		allowedDests := src.Destinations
		var relevantDests []destRuns
		for _, dr := range allDestRuns {
			isAllowed := len(allowedDests) == 0
			if !isAllowed {
				for _, ad := range allowedDests {
					if ad == dr.name {
						isAllowed = true
						break
					}
				}
			}
			if isAllowed {
				relevantDests = append(relevantDests, dr)
			}
		}

		if len(relevantDests) < 2 {
			continue
		}

		// Per-destination earliest timestamp for THIS target. A destination
		// added later (or repointed to a new target) legitimately won't
		// hold runs older than its own onboarding — flagging those as
		// "missing from <new-dest>" would just be noise after every dest
		// addition. earliest[destName] == "" means the destination has no
		// run for this target at all; that's a different problem
		// (configuration / permission) and we surface it separately.
		earliestPerDest := make(map[string]string, len(relevantDests))
		allTS := map[string]bool{}
		for _, dr := range relevantDests {
			for key := range dr.timestamps {
				if !strings.HasPrefix(key, src.Name+"@") {
					continue
				}
				ts := strings.TrimPrefix(key, src.Name+"@")
				allTS[ts] = true
				if cur, ok := earliestPerDest[dr.name]; !ok || ts < cur {
					earliestPerDest[dr.name] = ts
				}
			}
		}

		for ts := range allTS {
			var present, missing []string
			for _, dr := range relevantDests {
				if dr.timestamps[src.Name+"@"+ts] {
					present = append(present, dr.name)
					continue
				}
				// Skip "missing" if this destination has no runs for the
				// target at all (different problem class), or if its
				// earliest run is newer than this timestamp (it wasn't
				// receiving the target back then).
				earliest, has := earliestPerDest[dr.name]
				if !has || ts < earliest {
					continue
				}
				missing = append(missing, dr.name)
			}
			if len(missing) > 0 && len(present) > 0 {
				issues = append(issues, consistencyIssue{
					Target:      src.Name,
					Timestamp:   ts,
					PresentIn:   present,
					MissingFrom: missing,
				})
			}
		}
	}
	// Deterministic newest-first ordering. The map-range above produced
	// random order; pagination (slice(0,20) in the UI) was unstable
	// between refreshes, so the user kept seeing different rows for the
	// same data. Sort once here, never twice.
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Timestamp != issues[j].Timestamp {
			return issues[i].Timestamp > issues[j].Timestamp
		}
		return issues[i].Target < issues[j].Target
	})
	writeJSON(w, http.StatusOK, issues)
}

// Input validation helpers (validateK8sName, validatePort,
// isSupportedDBType, validateCronSchedule) → see validation.go

// uiReportingInstance returns a stable identifier for this operator pod
// used as Event.ReportingInstance. K8s validates the field as non-empty
// whenever EventTime is set, so this falls back gracefully if HOSTNAME
// (set by the kubelet inside pods) is somehow unavailable.
func uiReportingInstance() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "backup-operator-ui"
}

// emitUIEvent records a Kubernetes Event against any object the UI mutates
// (BackupSource, BackupDestination, BackupRecipient). Best-effort — failing to emit must not abort the
// mutation. The events.k8s.io/v1 schema requires ReportingController,
// ReportingInstance, and Action whenever EventTime is set; omitting any of
// them produces a "Required value" rejection from the API server and the
// audit trail goes dark.
func (s *Server) emitUIEvent(ctx context.Context, ref corev1.ObjectReference, reason, message string) {
	now := metav1.Now()
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: ref.Name + ".",
			Namespace:    ref.Namespace,
		},
		InvolvedObject: ref,
		Reason:         reason,
		// The auth-proxy identity and request id ride along in the message
		// (Event has no principal field), so the audit trail answers "who".
		Message:             message + auditSuffix(ctx),
		Type:                corev1.EventTypeNormal,
		Source:              corev1.EventSource{Component: "backup-operator-ui"},
		EventTime:           metav1.NewMicroTime(time.Now()),
		FirstTimestamp:      now,
		LastTimestamp:       now,
		Count:               1,
		ReportingController: "backup-operator-ui",
		ReportingInstance:   uiReportingInstance(),
		Action:              reason, // reasons are already short verb-like ("DestinationUpdated"); double-duty here is fine
	}
	if err := s.cfg.Client.Create(ctx, event); err != nil {
		s.cfg.Logger.Error(err, "emit ui event", "reason", reason, "kind", ref.Kind, "name", ref.Name)
	}
}

// emitObjectEvent records a Kubernetes Event against a backup.mogenius.io
// object the UI mutated so CRUD operations appear in `kubectl describe` and
// in the audit log served by /api/audit-log.
func (s *Server) emitObjectEvent(ctx context.Context, obj client.Object, kind, reason, message string) {
	s.emitUIEvent(ctx, corev1.ObjectReference{
		Kind:       kind,
		Namespace:  obj.GetNamespace(),
		Name:       obj.GetName(),
		UID:        obj.GetUID(),
		APIVersion: v1alpha1.GroupVersion.String(),
	}, reason, message)
}

// periodicRefresh polls Kubernetes for state changes and broadcasts SSE events.
func (s *Server) periodicRefresh(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.broadcast(sseEvent{Type: "refresh", Data: time.Now().Format(time.RFC3339)})
		}
	}
}
