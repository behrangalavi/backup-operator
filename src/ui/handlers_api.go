package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"backup-operator/internal/labels"

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
