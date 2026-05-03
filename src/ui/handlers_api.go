package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"backup-operator/internal/labels"
	"backup-operator/internal/meta"
	"backup-operator/internal/secrets"
	storageFactory "backup-operator/storage/factory"

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
	Destinations       string            `json:"destinations"`
	RetentionDays      string            `json:"retentionDays"`
	MinKeep            string            `json:"minKeep"`
	RowDropThreshold   string            `json:"rowDropThreshold"`
	SizeDropThreshold  string            `json:"sizeDropThreshold"`
	AnonymizeTables    *bool             `json:"anonymizeTables"`
	Extra              map[string]string `json:"extra"`
}

type destinationRequest struct {
	Name        string            `json:"name"`
	StorageType string            `json:"storageType"`
	PathPrefix  string            `json:"pathPrefix"`
	Data        map[string]string `json:"data"`
}

type apiResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
	Name    string `json:"name,omitempty"`
}

// --- Source CRUD ---

func (s *Server) handleAPICreateSource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "POST required"})
		return
	}
	var req sourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "invalid JSON: " + err.Error()})
		return
	}
	if req.Name == "" || req.DBType == "" || req.Host == "" || req.Username == "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "name, dbType, host, username are required"})
		return
	}
	if req.DBType != "postgres" && req.DBType != "mysql" && req.DBType != "mongo" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "dbType must be postgres, mysql, or mongo"})
		return
	}
	if msg := validateK8sName(req.Name); msg != "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: msg})
		return
	}
	if req.Port != "" {
		if msg := validatePort(req.Port); msg != "" {
			writeJSON(w, http.StatusBadRequest, apiResponse{Message: msg})
			return
		}
	}
	if req.Schedule != "" {
		if msg := validateCronSchedule(req.Schedule); msg != "" {
			writeJSON(w, http.StatusBadRequest, apiResponse{Message: msg})
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
		writeJSON(w, http.StatusConflict, apiResponse{Message: "failed to create: " + sanitizeError(err)})
		return
	}
	s.broadcast(sseEvent{Type: "source_created", Data: req.Name})
	writeJSON(w, http.StatusCreated, apiResponse{OK: true, Name: secretName})
}

func (s *Server) handleAPIUpdateSource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "PUT required"})
		return
	}
	secretName := trimPrefixPath(r.URL.Path, "/api/sources/")
	if secretName == "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "secret name required"})
		return
	}

	var req sourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "invalid JSON"})
		return
	}

	existing := &corev1.Secret{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: secretName}, existing); err != nil {
		writeJSON(w, http.StatusNotFound, apiResponse{Message: "secret not found"})
		return
	}
	if existing.Labels[labels.LabelRole] != labels.RoleSource {
		writeJSON(w, http.StatusForbidden, apiResponse{Message: "not a backup source secret"})
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
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "update failed"})
		return
	}
	s.broadcast(sseEvent{Type: "source_updated", Data: secretName})
	writeJSON(w, http.StatusOK, apiResponse{OK: true, Name: secretName})
}

func (s *Server) handleAPIDeleteSource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "DELETE required"})
		return
	}
	secretName := trimPrefixPath(r.URL.Path, "/api/sources/")
	if secretName == "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "secret name required"})
		return
	}

	existing := &corev1.Secret{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: secretName}, existing); err != nil {
		writeJSON(w, http.StatusNotFound, apiResponse{Message: "not found or already deleted"})
		return
	}
	if existing.Labels[labels.LabelRole] != labels.RoleSource {
		writeJSON(w, http.StatusForbidden, apiResponse{Message: "not a backup source secret"})
		return
	}
	if err := s.cfg.Client.Delete(r.Context(), existing); err != nil {
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "delete failed"})
		return
	}
	s.broadcast(sseEvent{Type: "source_deleted", Data: secretName})
	writeJSON(w, http.StatusOK, apiResponse{OK: true})
}

// --- Destination CRUD ---

func (s *Server) handleAPIListDestinations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "GET required"})
		return
	}
	var list corev1.SecretList
	if err := s.cfg.Client.List(r.Context(), &list, client.InNamespace(s.cfg.Namespace), client.MatchingLabels{
		labels.LabelRole: labels.RoleDestination,
	}); err != nil {
		s.cfg.Logger.Error(err, "list destination secrets")
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "internal error"})
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
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "POST required"})
		return
	}
	var req destinationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "invalid JSON"})
		return
	}
	if req.Name == "" || req.StorageType == "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "name and storageType are required"})
		return
	}
	if req.StorageType != "sftp" && req.StorageType != "hetzner-sftp" && req.StorageType != "s3" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "storageType must be sftp, hetzner-sftp, or s3"})
		return
	}
	if msg := validateK8sName(req.Name); msg != "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: msg})
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
		writeJSON(w, http.StatusConflict, apiResponse{Message: "failed to create: " + sanitizeError(err)})
		return
	}
	s.broadcast(sseEvent{Type: "destination_created", Data: req.Name})
	writeJSON(w, http.StatusCreated, apiResponse{OK: true, Name: secretName})
}

func (s *Server) handleAPIUpdateDestination(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "PUT required"})
		return
	}
	secretName := trimPrefixPath(r.URL.Path, "/api/destinations/")
	if secretName == "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "secret name required"})
		return
	}

	var req destinationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "invalid JSON"})
		return
	}

	existing := &corev1.Secret{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: secretName}, existing); err != nil {
		writeJSON(w, http.StatusNotFound, apiResponse{Message: "secret not found"})
		return
	}
	if existing.Labels[labels.LabelRole] != labels.RoleDestination {
		writeJSON(w, http.StatusForbidden, apiResponse{Message: "not a backup destination secret"})
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
	if req.PathPrefix != "" {
		if existing.Annotations == nil {
			existing.Annotations = make(map[string]string)
		}
		existing.Annotations[labels.AnnotationPathPrefix] = req.PathPrefix
	}
	for k, v := range req.Data {
		if existing.Data == nil {
			existing.Data = make(map[string][]byte)
		}
		existing.Data[k] = []byte(v)
	}

	if err := s.cfg.Client.Update(r.Context(), existing); err != nil {
		s.cfg.Logger.Error(err, "update destination secret")
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "update failed"})
		return
	}
	s.broadcast(sseEvent{Type: "destination_updated", Data: secretName})
	writeJSON(w, http.StatusOK, apiResponse{OK: true, Name: secretName})
}

func (s *Server) handleAPIDeleteDestination(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "DELETE required"})
		return
	}
	secretName := trimPrefixPath(r.URL.Path, "/api/destinations/")
	if secretName == "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "secret name required"})
		return
	}

	existing := &corev1.Secret{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: secretName}, existing); err != nil {
		writeJSON(w, http.StatusNotFound, apiResponse{Message: "not found or already deleted"})
		return
	}
	if existing.Labels[labels.LabelRole] != labels.RoleDestination {
		writeJSON(w, http.StatusForbidden, apiResponse{Message: "not a backup destination secret"})
		return
	}
	if err := s.cfg.Client.Delete(r.Context(), existing); err != nil {
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "delete failed"})
		return
	}
	s.broadcast(sseEvent{Type: "destination_deleted", Data: secretName})
	writeJSON(w, http.StatusOK, apiResponse{OK: true})
}

// --- Manual backup trigger ---

func (s *Server) handleAPITriggerBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "POST required"})
		return
	}
	targetName := trimPrefixPath(r.URL.Path, "/api/trigger/")
	if targetName == "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "target name required"})
		return
	}

	// Find the CronJob for this target.
	var cronJobs batchv1.CronJobList
	if err := s.cfg.Client.List(r.Context(), &cronJobs, client.InNamespace(s.cfg.Namespace)); err != nil {
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "failed to list cronjobs"})
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
		writeJSON(w, http.StatusNotFound, apiResponse{Message: "no cronjob found for target"})
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
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "failed to create job"})
		return
	}
	s.broadcast(sseEvent{Type: "backup_triggered", Data: targetName})
	writeJSON(w, http.StatusCreated, apiResponse{OK: true, Name: jobName, Message: "backup job created"})
}

// --- Job status ---

func (s *Server) handleAPIJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "GET required"})
		return
	}

	var jobs batchv1.JobList
	if err := s.cfg.Client.List(r.Context(), &jobs, client.InNamespace(s.cfg.Namespace), client.MatchingLabels{
		"app.kubernetes.io/managed-by": "backup-operator",
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "internal error"})
		return
	}

	type jobInfo struct {
		Name      string `json:"name"`
		Target    string `json:"target"`
		Status    string `json:"status"`
		StartTime string `json:"startTime,omitempty"`
		Duration  string `json:"duration,omitempty"`
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
	writeJSON(w, http.StatusOK, out)
}

// --- Server-Sent Events ---

type sseEvent struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

type sseBroker struct {
	mu      sync.Mutex
	clients map[chan sseEvent]struct{}
}

func newSSEBroker() *sseBroker {
	return &sseBroker{clients: make(map[chan sseEvent]struct{})}
}

func (b *sseBroker) subscribe() chan sseEvent {
	ch := make(chan sseEvent, 16)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *sseBroker) unsubscribe(ch chan sseEvent) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
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
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "SSE not supported"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := s.sse.subscribe()
	defer s.sse.unsubscribe(ch)

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
	if req.Destinations != "" {
		ann[labels.AnnotationDestinations] = req.Destinations
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
	if req.Destinations != "" {
		sec.Annotations[labels.AnnotationDestinations] = req.Destinations
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
	for k, v := range req.Extra {
		sec.Annotations["backup.mogenius.io/extra-"+k] = v
	}
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
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "GET required"})
		return
	}
	secretName := trimPrefixPath(r.URL.Path, "/api/sources/")
	if secretName == "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "secret name required"})
		return
	}

	sec := &corev1.Secret{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: secretName}, sec); err != nil {
		writeJSON(w, http.StatusNotFound, apiResponse{Message: "secret not found"})
		return
	}
	if sec.Labels[labels.LabelRole] != labels.RoleSource {
		writeJSON(w, http.StatusForbidden, apiResponse{Message: "not a backup source secret"})
		return
	}

	type sourceInfo struct {
		SecretName        string `json:"secretName"`
		Name              string `json:"name"`
		DBType            string `json:"dbType"`
		Schedule          string `json:"schedule"`
		Host              string `json:"host"`
		Port              string `json:"port"`
		Database          string `json:"database"`
		Username          string `json:"username"`
		HasPassword       bool   `json:"hasPassword"`
		AnalyzerEnabled   string `json:"analyzerEnabled"`
		Destinations      string `json:"destinations"`
		RetentionDays     string `json:"retentionDays"`
		MinKeep           string `json:"minKeep"`
		RowDropThreshold  string `json:"rowDropThreshold"`
		SizeDropThreshold string `json:"sizeDropThreshold"`
		AnonymizeTables   string `json:"anonymizeTables"`
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
		Host:              string(sec.Data["host"]),
		Port:              string(sec.Data["port"]),
		Database:          string(sec.Data["database"]),
		Username:          string(sec.Data["username"]),
		HasPassword:       len(sec.Data["password"]) > 0,
		AnalyzerEnabled:   sec.Annotations[labels.AnnotationAnalyzerEnabled],
		Destinations:      sec.Annotations[labels.AnnotationDestinations],
		RetentionDays:     sec.Annotations[labels.AnnotationRetentionDays],
		MinKeep:           sec.Annotations[labels.AnnotationMinKeep],
		RowDropThreshold:  sec.Annotations[labels.AnnotationRowDropThreshold],
		SizeDropThreshold: sec.Annotations[labels.AnnotationSizeDropThreshold],
		AnonymizeTables:   sec.Annotations[labels.AnnotationAnonymizeTables],
	})
}

// handleAPIGetDestination returns a single destination secret's configuration (without sensitive keys).
func (s *Server) handleAPIGetDestination(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "GET required"})
		return
	}
	secretName := trimPrefixPath(r.URL.Path, "/api/destinations/")
	if secretName == "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "secret name required"})
		return
	}

	sec := &corev1.Secret{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: secretName}, sec); err != nil {
		writeJSON(w, http.StatusNotFound, apiResponse{Message: "secret not found"})
		return
	}
	if sec.Labels[labels.LabelRole] != labels.RoleDestination {
		writeJSON(w, http.StatusForbidden, apiResponse{Message: "not a backup destination secret"})
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
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "POST required"})
		return
	}
	secretName := trimPrefixPath(r.URL.Path, "/api/destinations/")
	secretName = strings.TrimSuffix(secretName, "/test")

	var sec corev1.Secret
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: secretName}, &sec); err != nil {
		writeJSON(w, http.StatusNotFound, apiResponse{Message: "destination secret not found"})
		return
	}
	if sec.Labels[labels.LabelRole] != labels.RoleDestination {
		writeJSON(w, http.StatusForbidden, apiResponse{Message: "not a destination secret"})
		return
	}

	dest, err := secrets.ParseDestination(&sec)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "invalid destination config"})
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

	_, err = st.List(ctx, "__connectivity_test__/")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    false,
			"error": err.Error(),
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
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "GET required"})
		return
	}

	var list corev1.SecretList
	if err := s.cfg.Client.List(r.Context(), &list, client.InNamespace(s.cfg.Namespace), client.MatchingLabels{
		labels.LabelRole: labels.RoleDestination,
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "internal error"})
		return
	}

	results := make([]destStorageStats, len(list.Items))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range list.Items {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
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

			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
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
}

func (s *Server) handleAPIDestinationHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "GET required"})
		return
	}

	sources, err := s.data.listTargets(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "internal error"})
		return
	}

	var destList corev1.SecretList
	if err := s.cfg.Client.List(r.Context(), &destList, client.InNamespace(s.cfg.Namespace), client.MatchingLabels{
		labels.LabelRole: labels.RoleDestination,
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "internal error"})
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
			sem <- struct{}{}
			defer func() { <-sem }()

			st, err := storageFactory.NewStorage(d.StorageType, d.Name, d.Data, s.cfg.Logger.WithName("health"))
			if err != nil {
				mu.Lock()
				destLatest[d.Name] = lookupResult{err: err}
				mu.Unlock()
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
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
	writeJSON(w, http.StatusOK, entries)
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
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "GET required"})
		return
	}

	sources, err := s.data.listTargets(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "internal error"})
		return
	}

	var destList corev1.SecretList
	if err := s.cfg.Client.List(r.Context(), &destList, client.InNamespace(s.cfg.Namespace), client.MatchingLabels{
		labels.LabelRole: labels.RoleDestination,
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "internal error"})
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
			sem <- struct{}{}
			defer func() { <-sem }()

			st, err := storageFactory.NewStorage(d.StorageType, d.Name, d.Data, s.cfg.Logger.WithName("consistency"))
			if err != nil {
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
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

		// Collect all timestamps for this target across all destinations
		allTS := map[string]bool{}
		for _, dr := range relevantDests {
			for key := range dr.timestamps {
				if strings.HasPrefix(key, src.Name+"@") {
					ts := strings.TrimPrefix(key, src.Name+"@")
					allTS[ts] = true
				}
			}
		}

		for ts := range allTS {
			var present, missing []string
			for _, dr := range relevantDests {
				if dr.timestamps[src.Name+"@"+ts] {
					present = append(present, dr.name)
				} else {
					missing = append(missing, dr.name)
				}
			}
			if len(missing) > 0 {
				issues = append(issues, consistencyIssue{
					Target:      src.Name,
					Timestamp:   ts,
					PresentIn:   present,
					MissingFrom: missing,
				})
			}
		}
	}
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

// validateCronSchedule does basic structural validation of a cron expression.
// Accepts standard 5-field cron (minute hour dom month dow).
func validateCronSchedule(schedule string) string {
	fields := strings.Fields(schedule)
	if len(fields) != 5 {
		return "schedule must have exactly 5 fields (minute hour day-of-month month day-of-week)"
	}
	return ""
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
