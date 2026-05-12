package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"backup-operator/internal/labels"
	"backup-operator/internal/meta"
	"backup-operator/internal/safe"
	"backup-operator/internal/secrets"
	"backup-operator/metrics"
	storageFactory "backup-operator/storage/factory"

	dto "github.com/prometheus/client_model/go"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// --- JSON request/response types ---

type sourceRequest struct {
	Name               string            `json:"name"`
	DBType             string            `json:"dbType"`
	Schedule           string            `json:"schedule"`
	Host               string            `json:"host"`
	Port               string            `json:"port"`
	Database           string            `json:"database"`
	Username           string            `json:"username"`
	Password           string            `json:"password"`
	AnalyzerEnabled    *bool             `json:"analyzerEnabled"`
	EmptyDumpCheck     *bool             `json:"emptyDumpCheck"`
	Destinations       string            `json:"destinations"`
	RetentionDays      string            `json:"retentionDays"`
	MinKeep            string            `json:"minKeep"`
	RowDropThreshold   string            `json:"rowDropThreshold"`
	SizeDropThreshold  string            `json:"sizeDropThreshold"`
	AnonymizeTables    *bool             `json:"anonymizeTables"`
	// JitterMinutes spreads the cron's minute field per source.
	// Empty string = annotation absent (default behaviour: jitter when
	// minute==0 only). "0" pins the schedule. Any other integer is a
	// window override.
	JitterMinutes string `json:"jitterMinutes"`
	// Restore-verification (Phase 1 stream-validate, Phase 2 spawn-and-restore).
	// Empty string on optional fields means "don't change / use default".
	RestoreVerificationMode     string `json:"restoreVerificationMode"`
	RestoreVerificationInterval string `json:"restoreVerificationInterval"`
	VerificationImage           string `json:"verificationImage"`
	VerificationVolumeSize      string `json:"verificationVolumeSize"`
	Extra              map[string]string `json:"extra"`
}

type destinationRequest struct {
	Name        string            `json:"name"`
	StorageType string            `json:"storageType"`
	PathPrefix  string            `json:"pathPrefix"`
	Data        map[string]string `json:"data"`
	// RemoveKeys lists Secret data keys the caller wants explicitly dropped
	// on update. The merge semantics of PUT mean a missing key in `data`
	// preserves the existing value; this is the escape hatch when the user
	// genuinely wants to remove a field (e.g. switching SFTP auth from key
	// to password — the old ssh-private-key must go).
	RemoveKeys []string `json:"removeKeys,omitempty"`
}

type apiResponse struct {
	OK      bool   `json:"ok"`
	// Code is a stable machine-readable error tag — see errors_api.go for
	// the catalogue. Frontend uses it to pick toast severity and (later)
	// localised messages without parsing Message text.
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Name    string `json:"name,omitempty"`
}

// --- Source CRUD ---

func (s *Server) handleAPICreateSource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "POST required")
		return
	}
	var req sourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid JSON: " + err.Error())
		return
	}
	if req.Name == "" || req.DBType == "" || req.Host == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "name, dbType, host are required")
		return
	}
	// Redis can run with password-only AUTH (no username); every other DB
	// type still requires a username.
	if req.Username == "" && req.DBType != "redis" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "username is required for " + req.DBType)
		return
	}
	if !isSupportedDBType(req.DBType) {
		writeError(w, http.StatusBadRequest, codeBadRequest, "dbType must be postgres, mysql, mariadb, mongo, or redis")
		return
	}
	if msg := validateK8sName(req.Name); msg != "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, msg)
		return
	}
	if req.Port != "" {
		if msg := validatePort(req.Port); msg != "" {
			writeError(w, http.StatusBadRequest, codeBadRequest, msg)
			return
		}
	}
	if req.Schedule != "" {
		if msg := validateCronSchedule(req.Schedule); msg != "" {
			writeError(w, http.StatusBadRequest, codeBadRequest, msg)
			return
		}
	}

	secretName := "backup-src-" + sanitizeName(req.Name)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: s.cfg.Namespace,
			Labels: map[string]string{
				labels.LabelRole:   labels.RoleSource,
				labels.LabelDBType: req.DBType,
			},
			Annotations: buildSourceAnnotations(req),
		},
		Data: buildSourceData(req),
	}

	if err := s.cfg.Client.Create(r.Context(), secret); err != nil {
		s.cfg.Logger.Error(err, "create source secret")
		writeError(w, http.StatusConflict, codeConflict, "failed to create: " + sanitizeError(err))
		return
	}
	s.emitMutationEvent(r.Context(), secret, "SourceCreated", fmt.Sprintf("Source %q created via UI", req.Name))
	s.broadcast(sseEvent{Type: "source_created", Data: req.Name})
	writeJSON(w, http.StatusCreated, apiResponse{OK: true, Name: secretName})
}

func (s *Server) handleAPIUpdateSource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "PUT required")
		return
	}
	secretName := trimPrefixPath(r.URL.Path, "/api/sources/")
	if secretName == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "secret name required")
		return
	}

	var req sourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid JSON")
		return
	}

	existing := &corev1.Secret{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: secretName}, existing); err != nil {
		writeError(w, http.StatusNotFound, codeNotFound, "secret not found")
		return
	}
	if existing.Labels[labels.LabelRole] != labels.RoleSource {
		writeError(w, http.StatusForbidden, codeForbidden, "not a backup source secret")
		return
	}

	if req.DBType != "" {
		if existing.Labels == nil {
			existing.Labels = make(map[string]string)
		}
		existing.Labels[labels.LabelDBType] = req.DBType
	}
	mergeSourceAnnotations(existing, req)
	mergeSourceData(existing, req)

	if err := s.cfg.Client.Update(r.Context(), existing); err != nil {
		s.cfg.Logger.Error(err, "update source secret")
		writeError(w, http.StatusInternalServerError, codeInternal, "update failed")
		return
	}
	s.emitMutationEvent(r.Context(), existing, "SourceUpdated", fmt.Sprintf("Source %q updated via UI", secretName))
	s.broadcast(sseEvent{Type: "source_updated", Data: secretName})
	writeJSON(w, http.StatusOK, apiResponse{OK: true, Name: secretName})
}

func (s *Server) handleAPIDeleteSource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "DELETE required")
		return
	}
	secretName := trimPrefixPath(r.URL.Path, "/api/sources/")
	if secretName == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "secret name required")
		return
	}

	existing := &corev1.Secret{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: secretName}, existing); err != nil {
		writeError(w, http.StatusNotFound, codeNotFound, "not found or already deleted")
		return
	}
	if existing.Labels[labels.LabelRole] != labels.RoleSource {
		writeError(w, http.StatusForbidden, codeForbidden, "not a backup source secret")
		return
	}
	if err := s.cfg.Client.Delete(r.Context(), existing); err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "delete failed")
		return
	}
	s.emitMutationEvent(r.Context(), existing, "SourceDeleted", fmt.Sprintf("Source %q deleted via UI", secretName))
	s.broadcast(sseEvent{Type: "source_deleted", Data: secretName})
	writeJSON(w, http.StatusOK, apiResponse{OK: true})
}

// handleAPISuspendSource toggles the source's suspended annotation. The
// reconciler observes the change and writes Spec.Suspend on the managed
// CronJob. In-flight Jobs are unaffected — Suspend only blocks future ticks,
// which matches K8s semantics. Body: {"suspend": true|false}.
//
// Separate from the regular update endpoint so a one-click pause does not
// require sending the whole source body (and risk overwriting a concurrent
// edit). The annotation is removed entirely on resume so cleared sources
// don't carry a stale "suspended=false".
func (s *Server) handleAPISuspendSource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "POST required")
		return
	}
	rest := trimPrefixPath(r.URL.Path, "/api/sources/")
	secretName := strings.TrimSuffix(rest, "/suspend")
	if secretName == "" || strings.Contains(secretName, "/") {
		writeError(w, http.StatusBadRequest, codeBadRequest, "secret name required")
		return
	}

	var body struct {
		Suspend bool `json:"suspend"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid JSON")
		return
	}

	existing := &corev1.Secret{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: secretName}, existing); err != nil {
		writeError(w, http.StatusNotFound, codeNotFound, "secret not found")
		return
	}
	if existing.Labels[labels.LabelRole] != labels.RoleSource {
		writeError(w, http.StatusForbidden, codeForbidden, "not a backup source secret")
		return
	}

	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	if body.Suspend {
		existing.Annotations[labels.AnnotationSuspended] = "true"
	} else {
		delete(existing.Annotations, labels.AnnotationSuspended)
	}

	if err := s.cfg.Client.Update(r.Context(), existing); err != nil {
		s.cfg.Logger.Error(err, "patch suspend annotation")
		writeError(w, http.StatusInternalServerError, codeInternal, "update failed")
		return
	}
	evType := "source_resumed"
	reason := "SourceUpdated"
	action := "resumed"
	if body.Suspend {
		evType = "source_suspended"
		action = "suspended"
	}
	s.emitMutationEvent(r.Context(), existing, reason, fmt.Sprintf("Source %q %s via UI", secretName, action))
	s.broadcast(sseEvent{Type: evType, Data: secretName})
	writeJSON(w, http.StatusOK, apiResponse{OK: true, Name: secretName})
}

// --- Destination CRUD ---

func (s *Server) handleAPIListDestinations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "GET required")
		return
	}
	var list corev1.SecretList
	if err := s.cfg.Client.List(r.Context(), &list, client.InNamespace(s.cfg.Namespace), client.MatchingLabels{
		labels.LabelRole: labels.RoleDestination,
	}); err != nil {
		s.cfg.Logger.Error(err, "list destination secrets")
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	type destInfo struct {
		SecretName  string `json:"secretName"`
		Name        string `json:"name"`
		StorageType string `json:"storageType"`
		PathPrefix  string `json:"pathPrefix"`
		Host        string `json:"host"`
		CreatedAt   string `json:"createdAt,omitempty"`
	}
	out := make([]destInfo, 0, len(list.Items))
	for _, sec := range list.Items {
		name := sec.Annotations[labels.AnnotationName]
		if name == "" {
			name = sec.Name
		}
		var createdAt string
		if !sec.CreationTimestamp.IsZero() {
			createdAt = sec.CreationTimestamp.UTC().Format(time.RFC3339)
		}
		out = append(out, destInfo{
			SecretName:  sec.Name,
			Name:        name,
			StorageType: sec.Labels[labels.LabelStorageType],
			PathPrefix:  sec.Annotations[labels.AnnotationPathPrefix],
			Host:        string(sec.Data["host"]),
			CreatedAt:   createdAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAPICreateDestination(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "POST required")
		return
	}
	var req destinationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid JSON")
		return
	}
	if req.Name == "" || req.StorageType == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "name and storageType are required")
		return
	}
	if req.StorageType != "sftp" && req.StorageType != "hetzner-sftp" && req.StorageType != "s3" && req.StorageType != "ftps" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "storageType must be sftp, hetzner-sftp, ftps, or s3")
		return
	}
	if msg := validateK8sName(req.Name); msg != "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, msg)
		return
	}

	secretName := "backup-dest-" + sanitizeName(req.Name)
	annotations := map[string]string{
		labels.AnnotationName: req.Name,
	}
	if req.PathPrefix != "" {
		annotations[labels.AnnotationPathPrefix] = req.PathPrefix
	}

	data := make(map[string][]byte, len(req.Data))
	for k, v := range req.Data {
		data[k] = []byte(v)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        secretName,
			Namespace:   s.cfg.Namespace,
			Labels: map[string]string{
				labels.LabelRole:        labels.RoleDestination,
				labels.LabelStorageType: req.StorageType,
			},
			Annotations: annotations,
		},
		Data: data,
	}

	if err := s.cfg.Client.Create(r.Context(), secret); err != nil {
		s.cfg.Logger.Error(err, "create destination secret")
		writeError(w, http.StatusConflict, codeConflict, "failed to create: " + sanitizeError(err))
		return
	}
	s.emitMutationEvent(r.Context(), secret, "DestinationCreated", fmt.Sprintf("Destination %q created via UI", req.Name))
	s.broadcast(sseEvent{Type: "destination_created", Data: req.Name})
	writeJSON(w, http.StatusCreated, apiResponse{OK: true, Name: secretName})
}

func (s *Server) handleAPIUpdateDestination(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "PUT required")
		return
	}
	secretName := trimPrefixPath(r.URL.Path, "/api/destinations/")
	if secretName == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "secret name required")
		return
	}

	var req destinationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid JSON")
		return
	}

	existing := &corev1.Secret{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: secretName}, existing); err != nil {
		writeError(w, http.StatusNotFound, codeNotFound, "secret not found")
		return
	}
	if existing.Labels[labels.LabelRole] != labels.RoleDestination {
		writeError(w, http.StatusForbidden, codeForbidden, "not a backup destination secret")
		return
	}

	if req.StorageType != "" {
		if existing.Labels == nil {
			existing.Labels = make(map[string]string)
		}
		existing.Labels[labels.LabelStorageType] = req.StorageType
	}
	if req.Name != "" {
		if existing.Annotations == nil {
			existing.Annotations = make(map[string]string)
		}
		existing.Annotations[labels.AnnotationName] = req.Name
	}
	// PathPrefix follows three-valued logic on update: a non-empty value
	// replaces, an empty string explicitly clears the annotation, and a
	// missing field (would require the JSON to omit it) preserves. The
	// UI always sends pathPrefix so the empty-string→clear branch covers
	// the "user cleared the input box" case.
	if req.PathPrefix != "" {
		if existing.Annotations == nil {
			existing.Annotations = make(map[string]string)
		}
		existing.Annotations[labels.AnnotationPathPrefix] = req.PathPrefix
	} else if existing.Annotations != nil {
		delete(existing.Annotations, labels.AnnotationPathPrefix)
	}
	for k, v := range req.Data {
		if existing.Data == nil {
			existing.Data = make(map[string][]byte)
		}
		existing.Data[k] = []byte(v)
	}
	for _, k := range req.RemoveKeys {
		delete(existing.Data, k)
	}

	if err := s.cfg.Client.Update(r.Context(), existing); err != nil {
		s.cfg.Logger.Error(err, "update destination secret")
		writeError(w, http.StatusInternalServerError, codeInternal, "update failed")
		return
	}
	s.emitMutationEvent(r.Context(), existing, "DestinationUpdated", fmt.Sprintf("Destination %q updated via UI", secretName))
	s.broadcast(sseEvent{Type: "destination_updated", Data: secretName})
	writeJSON(w, http.StatusOK, apiResponse{OK: true, Name: secretName})
}

func (s *Server) handleAPIDeleteDestination(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "DELETE required")
		return
	}
	secretName := trimPrefixPath(r.URL.Path, "/api/destinations/")
	if secretName == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "secret name required")
		return
	}

	existing := &corev1.Secret{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: secretName}, existing); err != nil {
		writeError(w, http.StatusNotFound, codeNotFound, "not found or already deleted")
		return
	}
	if existing.Labels[labels.LabelRole] != labels.RoleDestination {
		writeError(w, http.StatusForbidden, codeForbidden, "not a backup destination secret")
		return
	}
	if err := s.cfg.Client.Delete(r.Context(), existing); err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "delete failed")
		return
	}
	s.emitMutationEvent(r.Context(), existing, "DestinationDeleted", fmt.Sprintf("Destination %q deleted via UI", secretName))
	s.broadcast(sseEvent{Type: "destination_deleted", Data: secretName})
	writeJSON(w, http.StatusOK, apiResponse{OK: true})
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
	if err := s.cfg.Client.List(r.Context(), &cronJobs, client.InNamespace(s.cfg.Namespace)); err != nil {
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
		Name                      string  `json:"name"`
		Target                    string  `json:"target"`
		Status                    string  `json:"status"`
		StartTime                 string  `json:"startTime,omitempty"`
		Duration                  string  `json:"duration,omitempty"`
		EstimatedDurationSeconds  float64 `json:"estimatedDurationSeconds,omitempty"`
		EstimateSampleSize        int     `json:"estimateSampleSize,omitempty"`
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

	writeJSON(w, http.StatusOK, out)
}

// --- Server-Sent Events ---

type sseEvent struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

type sseBroker struct {
	mu         sync.Mutex
	clients    map[chan sseEvent]struct{}
	maxClients int // 0 = unlimited
}

func newSSEBroker() *sseBroker {
	return &sseBroker{clients: make(map[chan sseEvent]struct{})}
}

// subscribe registers a new SSE client. Returns nil when the broker is at
// maxClients capacity, in which case the caller MUST refuse the connection
// rather than block — otherwise an unauthenticated client (UI auth is
// pluggable) can pin operator memory by hoarding subscriptions.
func (b *sseBroker) subscribe() chan sseEvent {
	ch := make(chan sseEvent, 16)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.maxClients > 0 && len(b.clients) >= b.maxClients {
		close(ch)
		return nil
	}
	b.clients[ch] = struct{}{}
	return ch
}

func (b *sseBroker) unsubscribe(ch chan sseEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.clients[ch]; !ok {
		return
	}
	delete(b.clients, ch)
	close(ch)
}

func (b *sseBroker) publish(ev sseEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (s *Server) broadcast(ev sseEvent) {
	if s.sse != nil {
		s.sse.publish(ev)
	}
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, codeInternal, "SSE not supported")
		return
	}

	ch := s.sse.subscribe()
	if ch == nil {
		// Broker is full. Refuse explicitly so the client retries later
		// rather than holding a half-open SSE stream that pins memory.
		writeError(w, http.StatusServiceUnavailable, codeInternal, "too many SSE clients; retry shortly")
		return
	}
	defer s.sse.unsubscribe(ch)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Send initial ping.
	_, _ = fmt.Fprintf(w, "event: connected\ndata: ok\n\n")
	flusher.Flush()

	ctx := r.Context()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			data, _ := json.Marshal(ev)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
			flusher.Flush()
		case <-ticker.C:
			_, _ = fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

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

func buildSourceAnnotations(req sourceRequest) map[string]string {
	ann := map[string]string{
		labels.AnnotationName: req.Name,
	}
	if req.Schedule != "" {
		ann[labels.AnnotationSchedule] = req.Schedule
	}
	if req.AnalyzerEnabled != nil {
		ann[labels.AnnotationAnalyzerEnabled] = fmt.Sprintf("%t", *req.AnalyzerEnabled)
	}
	if req.EmptyDumpCheck != nil {
		ann[labels.AnnotationEmptyDumpCheck] = fmt.Sprintf("%t", *req.EmptyDumpCheck)
	}
	if req.Destinations != "" {
		ann[labels.AnnotationDestinations] = req.Destinations
	}
	if req.JitterMinutes != "" {
		ann[labels.AnnotationJitterMinutes] = req.JitterMinutes
	}
	if req.RetentionDays != "" {
		ann[labels.AnnotationRetentionDays] = req.RetentionDays
	}
	if req.MinKeep != "" {
		ann[labels.AnnotationMinKeep] = req.MinKeep
	}
	if req.RowDropThreshold != "" {
		ann[labels.AnnotationRowDropThreshold] = req.RowDropThreshold
	}
	if req.SizeDropThreshold != "" {
		ann[labels.AnnotationSizeDropThreshold] = req.SizeDropThreshold
	}
	if req.AnonymizeTables != nil && *req.AnonymizeTables {
		ann[labels.AnnotationAnonymizeTables] = "true"
	}
	if req.RestoreVerificationMode != "" {
		ann[labels.AnnotationRestoreVerificationMode] = req.RestoreVerificationMode
	}
	if req.RestoreVerificationInterval != "" {
		ann[labels.AnnotationRestoreVerificationInterval] = req.RestoreVerificationInterval
	}
	if req.VerificationImage != "" {
		ann[labels.AnnotationVerificationImage] = req.VerificationImage
	}
	if req.VerificationVolumeSize != "" {
		ann[labels.AnnotationVerificationVolumeSize] = req.VerificationVolumeSize
	}
	for k, v := range req.Extra {
		ann["backup.mogenius.io/extra-"+k] = v
	}
	return ann
}

func buildSourceData(req sourceRequest) map[string][]byte {
	data := map[string][]byte{
		"host":     []byte(req.Host),
		"username": []byte(req.Username),
	}
	if req.Port != "" {
		data["port"] = []byte(req.Port)
	}
	if req.Database != "" {
		data["database"] = []byte(req.Database)
	}
	if req.Password != "" {
		data["password"] = []byte(req.Password)
	}
	return data
}

func mergeSourceAnnotations(sec *corev1.Secret, req sourceRequest) {
	if sec.Annotations == nil {
		sec.Annotations = make(map[string]string)
	}
	if req.Name != "" {
		sec.Annotations[labels.AnnotationName] = req.Name
	}
	if req.Schedule != "" {
		sec.Annotations[labels.AnnotationSchedule] = req.Schedule
	}
	if req.AnalyzerEnabled != nil {
		sec.Annotations[labels.AnnotationAnalyzerEnabled] = fmt.Sprintf("%t", *req.AnalyzerEnabled)
	}
	if req.EmptyDumpCheck != nil {
		sec.Annotations[labels.AnnotationEmptyDumpCheck] = fmt.Sprintf("%t", *req.EmptyDumpCheck)
	}
	if req.Destinations != "" {
		sec.Annotations[labels.AnnotationDestinations] = req.Destinations
	}
	if req.JitterMinutes != "" {
		sec.Annotations[labels.AnnotationJitterMinutes] = req.JitterMinutes
	}
	if req.RetentionDays != "" {
		sec.Annotations[labels.AnnotationRetentionDays] = req.RetentionDays
	}
	if req.MinKeep != "" {
		sec.Annotations[labels.AnnotationMinKeep] = req.MinKeep
	}
	if req.RowDropThreshold != "" {
		sec.Annotations[labels.AnnotationRowDropThreshold] = req.RowDropThreshold
	}
	if req.SizeDropThreshold != "" {
		sec.Annotations[labels.AnnotationSizeDropThreshold] = req.SizeDropThreshold
	}
	if req.AnonymizeTables != nil {
		sec.Annotations[labels.AnnotationAnonymizeTables] = fmt.Sprintf("%t", *req.AnonymizeTables)
	}
	// Verification annotations: empty string from the form means "unset"
	// (delete the annotation) so users can revert to default by clearing
	// the input. Non-empty replaces.
	applyOptionalAnnotation(sec, labels.AnnotationRestoreVerificationMode, req.RestoreVerificationMode)
	applyOptionalAnnotation(sec, labels.AnnotationRestoreVerificationInterval, req.RestoreVerificationInterval)
	applyOptionalAnnotation(sec, labels.AnnotationVerificationImage, req.VerificationImage)
	applyOptionalAnnotation(sec, labels.AnnotationVerificationVolumeSize, req.VerificationVolumeSize)
	for k, v := range req.Extra {
		sec.Annotations["backup.mogenius.io/extra-"+k] = v
	}
}

// applyOptionalAnnotation sets the annotation to v if non-empty, deletes
// it otherwise. Lets the form clear an annotation by submitting an empty
// string for that field. Without this, a cleared form field would have
// no effect (zero-valued string just means "skip").
func applyOptionalAnnotation(sec *corev1.Secret, key, v string) {
	if v == "" {
		delete(sec.Annotations, key)
		return
	}
	sec.Annotations[key] = v
}

func mergeSourceData(sec *corev1.Secret, req sourceRequest) {
	if sec.Data == nil {
		sec.Data = make(map[string][]byte)
	}
	if req.Host != "" {
		sec.Data["host"] = []byte(req.Host)
	}
	if req.Port != "" {
		sec.Data["port"] = []byte(req.Port)
	}
	if req.Database != "" {
		sec.Data["database"] = []byte(req.Database)
	}
	if req.Username != "" {
		sec.Data["username"] = []byte(req.Username)
	}
	if req.Password != "" {
		sec.Data["password"] = []byte(req.Password)
	}
}

// handleAPIGetSource returns a single source secret's configuration (without password).
func (s *Server) handleAPIGetSource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "GET required")
		return
	}
	secretName := trimPrefixPath(r.URL.Path, "/api/sources/")
	if secretName == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "secret name required")
		return
	}

	sec := &corev1.Secret{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: secretName}, sec); err != nil {
		writeError(w, http.StatusNotFound, codeNotFound, "secret not found")
		return
	}
	if sec.Labels[labels.LabelRole] != labels.RoleSource {
		writeError(w, http.StatusForbidden, codeForbidden, "not a backup source secret")
		return
	}

	type sourceInfo struct {
		SecretName        string `json:"secretName"`
		Name              string `json:"name"`
		DBType            string `json:"dbType"`
		Schedule          string `json:"schedule"`
		Suspended         bool   `json:"suspended"`
		Host              string `json:"host"`
		Port              string `json:"port"`
		Database          string `json:"database"`
		Username          string `json:"username"`
		HasPassword       bool   `json:"hasPassword"`
		AnalyzerEnabled   string `json:"analyzerEnabled"`
		EmptyDumpCheck    string `json:"emptyDumpCheck"`
		Destinations      string `json:"destinations"`
		JitterMinutes     string `json:"jitterMinutes"`
		RetentionDays     string `json:"retentionDays"`
		MinKeep           string `json:"minKeep"`
		RowDropThreshold  string `json:"rowDropThreshold"`
		SizeDropThreshold string `json:"sizeDropThreshold"`
		AnonymizeTables   string `json:"anonymizeTables"`
		// Restore-verification settings — empty when annotation absent.
		RestoreVerificationMode     string `json:"restoreVerificationMode"`
		RestoreVerificationInterval string `json:"restoreVerificationInterval"`
		VerificationImage           string `json:"verificationImage"`
		VerificationVolumeSize      string `json:"verificationVolumeSize"`
	}

	name := sec.Annotations[labels.AnnotationName]
	if name == "" {
		name = sec.Name
	}

	writeJSON(w, http.StatusOK, sourceInfo{
		SecretName:        sec.Name,
		Name:              name,
		DBType:            sec.Labels[labels.LabelDBType],
		Schedule:          sec.Annotations[labels.AnnotationSchedule],
		Suspended:         strings.EqualFold(strings.TrimSpace(sec.Annotations[labels.AnnotationSuspended]), "true"),
		Host:              string(sec.Data["host"]),
		Port:              string(sec.Data["port"]),
		Database:          string(sec.Data["database"]),
		Username:          string(sec.Data["username"]),
		HasPassword:       len(sec.Data["password"]) > 0,
		AnalyzerEnabled:   sec.Annotations[labels.AnnotationAnalyzerEnabled],
		EmptyDumpCheck:    sec.Annotations[labels.AnnotationEmptyDumpCheck],
		Destinations:      sec.Annotations[labels.AnnotationDestinations],
		JitterMinutes:     sec.Annotations[labels.AnnotationJitterMinutes],
		RetentionDays:     sec.Annotations[labels.AnnotationRetentionDays],
		MinKeep:           sec.Annotations[labels.AnnotationMinKeep],
		RowDropThreshold:  sec.Annotations[labels.AnnotationRowDropThreshold],
		SizeDropThreshold: sec.Annotations[labels.AnnotationSizeDropThreshold],
		AnonymizeTables:   sec.Annotations[labels.AnnotationAnonymizeTables],
		RestoreVerificationMode:     sec.Annotations[labels.AnnotationRestoreVerificationMode],
		RestoreVerificationInterval: sec.Annotations[labels.AnnotationRestoreVerificationInterval],
		VerificationImage:           sec.Annotations[labels.AnnotationVerificationImage],
		VerificationVolumeSize:      sec.Annotations[labels.AnnotationVerificationVolumeSize],
	})
}

// handleAPIGetDestination returns a single destination secret's configuration (without sensitive keys).
func (s *Server) handleAPIGetDestination(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "GET required")
		return
	}
	secretName := trimPrefixPath(r.URL.Path, "/api/destinations/")
	if secretName == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "secret name required")
		return
	}

	sec := &corev1.Secret{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: secretName}, sec); err != nil {
		writeError(w, http.StatusNotFound, codeNotFound, "secret not found")
		return
	}
	if sec.Labels[labels.LabelRole] != labels.RoleDestination {
		writeError(w, http.StatusForbidden, codeForbidden, "not a backup destination secret")
		return
	}

	name := sec.Annotations[labels.AnnotationName]
	if name == "" {
		name = sec.Name
	}

	safeData := make(map[string]string)
	sensitiveKeys := map[string]bool{
		"password": true, "ssh-private-key": true, "secret-key": true,
		"access-key": true, "secret-access-key": true,
	}
	for k, v := range sec.Data {
		if sensitiveKeys[k] {
			safeData[k] = "***"
		} else {
			safeData[k] = string(v)
		}
	}

	type destInfo struct {
		SecretName  string            `json:"secretName"`
		Name        string            `json:"name"`
		StorageType string            `json:"storageType"`
		PathPrefix  string            `json:"pathPrefix"`
		Data        map[string]string `json:"data"`
	}

	writeJSON(w, http.StatusOK, destInfo{
		SecretName:  sec.Name,
		Name:        name,
		StorageType: sec.Labels[labels.LabelStorageType],
		PathPrefix:  sec.Annotations[labels.AnnotationPathPrefix],
		Data:        safeData,
	})
}

// --- Destination connectivity test ---

func (s *Server) handleAPITestDestination(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "POST required")
		return
	}
	secretName := trimPrefixPath(r.URL.Path, "/api/destinations/")
	secretName = strings.TrimSuffix(secretName, "/test")

	var sec corev1.Secret
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: secretName}, &sec); err != nil {
		writeError(w, http.StatusNotFound, codeNotFound, "destination secret not found")
		return
	}
	if sec.Labels[labels.LabelRole] != labels.RoleDestination {
		writeError(w, http.StatusForbidden, codeForbidden, "not a destination secret")
		return
	}

	dest, err := secrets.ParseDestination(&sec)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid destination config")
		return
	}

	st, err := storageFactory.NewStorage(dest.StorageType, dest.Name, dest.Data, s.cfg.Logger.WithName("test-connection"))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    false,
			"error": "storage init failed: " + err.Error(),
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// Real write probe: upload a tiny object, read it back, delete it. List
	// alone only verifies dial+login, which masks the common failure of
	// "credentials valid but user has no write permission" — exactly the
	// case where backups will run forever without ever landing a byte.
	probePath := fmt.Sprintf(".backup-operator-probe-%d", time.Now().UnixNano())
	payload := []byte("backup-operator connectivity probe")
	if err := st.Upload(ctx, probePath, strings.NewReader(string(payload))); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    false,
			"error": "upload failed: " + err.Error(),
		})
		return
	}
	// Best-effort cleanup — even if Delete fails the destination is
	// still proven writable, but we want to leave no trace if possible.
	defer func() {
		delCtx, delCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer delCancel()
		_ = st.Delete(delCtx, probePath)
	}()
	rc, err := st.Get(ctx, probePath)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    false,
			"error": "readback failed: " + err.Error(),
		})
		return
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if string(got) != string(payload) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("readback mismatch: wrote %d bytes, got %d", len(payload), len(got)),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- Destination storage stats ---

type destStorageStats struct {
	Name         string `json:"name"`
	StorageType  string `json:"storageType"`
	TotalFiles   int    `json:"totalFiles"`
	TotalSizeBytes int64 `json:"totalSizeBytes"`
	BackupCount  int    `json:"backupCount"`
	MetaCount    int    `json:"metaCount"`
	OldestBackup string `json:"oldestBackup,omitempty"`
	NewestBackup string `json:"newestBackup,omitempty"`
	Error        string `json:"error,omitempty"`
}

func (s *Server) handleAPIDestinationStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "GET required")
		return
	}

	var list corev1.SecretList
	if err := s.cfg.Client.List(r.Context(), &list, client.InNamespace(s.cfg.Namespace), client.MatchingLabels{
		labels.LabelRole: labels.RoleDestination,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}

	results := make([]destStorageStats, len(list.Items))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range list.Items {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer safe.Goroutine(s.cfg.Logger, "destination-stats", list.Items[idx].Name)
			sem <- struct{}{}
			defer func() { <-sem }()

			sec := &list.Items[idx]
			dest, err := secrets.ParseDestination(sec)
			if err != nil {
				results[idx] = destStorageStats{
					Name:  sec.Annotations[labels.AnnotationName],
					Error: "invalid config",
				}
				return
			}

			st, err := storageFactory.NewStorage(dest.StorageType, dest.Name, dest.Data, s.cfg.Logger.WithName("stats"))
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
					Error:       err.Error(),
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
				} else if strings.HasSuffix(o.Path, ".sql.gz.age") || strings.HasSuffix(o.Path, ".archive.gz.age") {
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
	ScrubStatus       string `json:"scrubStatus,omitempty"`        // "ok" | "failed"
	ScrubLastCheck    int64  `json:"scrubLastCheck,omitempty"`     // unix seconds
	ScrubFailedTotal  int64  `json:"scrubFailedTotal,omitempty"`   // cumulative failures
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

	var destList corev1.SecretList
	if err := s.cfg.Client.List(r.Context(), &destList, client.InNamespace(s.cfg.Namespace), client.MatchingLabels{
		labels.LabelRole: labels.RoleDestination,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}

	dests := make([]*secrets.Destination, 0, len(destList.Items))
	for i := range destList.Items {
		d, err := secrets.ParseDestination(&destList.Items[i])
		if err != nil {
			continue
		}
		dests = append(dests, d)
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
			sem <- struct{}{}
			defer func() { <-sem }()

			st, err := storageFactory.NewStorage(d.StorageType, d.Name, d.Data, s.cfg.Logger.WithName("health"))
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
					entry.Error = lr.err.Error()
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
		case "backup_operator_storage_scrub_passed":
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
		case "backup_operator_storage_scrub_last_check_timestamp_seconds":
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
		case "backup_operator_storage_scrub_failed_total":
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

	var destList corev1.SecretList
	if err := s.cfg.Client.List(r.Context(), &destList, client.InNamespace(s.cfg.Namespace), client.MatchingLabels{
		labels.LabelRole: labels.RoleDestination,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}

	dests := make([]*secrets.Destination, 0, len(destList.Items))
	for i := range destList.Items {
		d, err := secrets.ParseDestination(&destList.Items[i])
		if err != nil {
			continue
		}
		dests = append(dests, d)
	}

	// Fetch runs per destination
	type destRuns struct {
		name      string
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
			sem <- struct{}{}
			defer func() { <-sem }()

			st, err := storageFactory.NewStorage(d.StorageType, d.Name, d.Data, s.cfg.Logger.WithName("consistency"))
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

// --- Input validation helpers ---

var k8sNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

func validateK8sName(name string) string {
	if len(name) > 253 {
		return "name must be at most 253 characters"
	}
	if !k8sNameRe.MatchString(name) {
		return "name must consist of lowercase alphanumeric characters, '-' or '.', and start/end with alphanumeric"
	}
	return ""
}

func validatePort(port string) string {
	p, err := strconv.Atoi(strings.TrimSpace(port))
	if err != nil {
		return "port must be a number"
	}
	if p < 1 || p > 65535 {
		return "port must be between 1 and 65535"
	}
	return ""
}

// isSupportedDBType is the single allow-list checked at the API edge. The
// dumper factory is the only other place that knows about DB types; the UI
// must stay in sync with it.
func isSupportedDBType(t string) bool {
	switch t {
	case "postgres", "mysql", "mariadb", "mongo", "redis":
		return true
	}
	return false
}

// validateCronSchedule does basic structural validation of a cron expression.
// Accepts standard 5-field cron (minute hour dom month dow).
func validateCronSchedule(schedule string) string {
	fields := strings.Fields(schedule)
	if len(fields) != 5 {
		return "schedule must have exactly 5 fields (minute hour day-of-month month day-of-week)"
	}
	return ""
}

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
// (Secret, ConfigMap). Best-effort — failing to emit must not abort the
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
		InvolvedObject:      ref,
		Reason:              reason,
		Message:             message,
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

// emitMutationEvent records a Kubernetes Event against the mutated Secret so
// CRUD operations appear in `kubectl describe secret` and in the audit log
// served by /api/audit-log.
func (s *Server) emitMutationEvent(ctx context.Context, sec *corev1.Secret, reason, message string) {
	s.emitUIEvent(ctx, corev1.ObjectReference{
		Kind:       "Secret",
		Namespace:  sec.Namespace,
		Name:       sec.Name,
		UID:        sec.UID,
		APIVersion: "v1",
	}, reason, message)
}

// emitConfigMapEvent records a Kubernetes Event against a ConfigMap. Used for
// settings mutations so they appear in the audit log alongside Secret changes.
func (s *Server) emitConfigMapEvent(ctx context.Context, cm *corev1.ConfigMap, reason, message string) {
	s.emitUIEvent(ctx, corev1.ObjectReference{
		Kind:       "ConfigMap",
		Namespace:  cm.Namespace,
		Name:       cm.Name,
		UID:        cm.UID,
		APIVersion: "v1",
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
