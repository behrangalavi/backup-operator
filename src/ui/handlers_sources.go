package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"backup-operator/api/v1alpha1"
)

// sourceRequest is the JSON body for POST /api/sources and PUT
// /api/sources/{name}. The shape predates the CRD move and is kept so the
// SPA's form did not have to change: the handler translates it into a
// BackupSource spec plus a password-only credentials Secret. Empty strings
// mean "unset / use default" on optional fields.
type sourceRequest struct {
	Name              string `json:"name"`
	DBType            string `json:"dbType"`
	Schedule          string `json:"schedule"`
	Host              string `json:"host"`
	Port              string `json:"port"`
	Database          string `json:"database"`
	Username          string `json:"username"`
	Password          string `json:"password"`
	AnalyzerEnabled   *bool  `json:"analyzerEnabled"`
	EmptyDumpCheck    *bool  `json:"emptyDumpCheck"`
	Destinations      string `json:"destinations"`
	RetentionDays     string `json:"retentionDays"`
	MinKeep           string `json:"minKeep"`
	RowDropThreshold  string `json:"rowDropThreshold"`
	SizeDropThreshold string `json:"sizeDropThreshold"`
	AnonymizeTables   *bool  `json:"anonymizeTables"`
	// JitterMinutes: "" = unset (jitter when minute==0 only), "0" pins the
	// schedule, any other integer is an explicit window.
	JitterMinutes               string            `json:"jitterMinutes"`
	RestoreVerificationMode     string            `json:"restoreVerificationMode"`
	RestoreVerificationInterval string            `json:"restoreVerificationInterval"`
	VerificationImage           string            `json:"verificationImage"`
	VerificationVolumeSize      string            `json:"verificationVolumeSize"`
	Compression                 string            `json:"compression"`
	Extra                       map[string]string `json:"extra"`
}

// sourceInfo is the GET /api/sources/{name} response. Optional fields come
// back as strings exactly like the form submits them ("" = unset) so the
// edit form can round-trip without knowing about pointers.
type sourceInfo struct {
	ObjectName                  string `json:"objectName"`
	Name                        string `json:"name"`
	TargetName                  string `json:"targetName"`
	DBType                      string `json:"dbType"`
	Schedule                    string `json:"schedule"`
	Suspended                   bool   `json:"suspended"`
	Host                        string `json:"host"`
	Port                        string `json:"port"`
	Database                    string `json:"database"`
	Username                    string `json:"username"`
	CredentialsSecret           string `json:"credentialsSecret"`
	HasPassword                 bool   `json:"hasPassword"`
	AnalyzerEnabled             string `json:"analyzerEnabled"`
	EmptyDumpCheck              string `json:"emptyDumpCheck"`
	Destinations                string `json:"destinations"`
	JitterMinutes               string `json:"jitterMinutes"`
	RetentionDays               string `json:"retentionDays"`
	MinKeep                     string `json:"minKeep"`
	RowDropThreshold            string `json:"rowDropThreshold"`
	SizeDropThreshold           string `json:"sizeDropThreshold"`
	AnonymizeTables             string `json:"anonymizeTables"`
	RestoreVerificationMode     string `json:"restoreVerificationMode"`
	RestoreVerificationInterval string `json:"restoreVerificationInterval"`
	VerificationImage           string `json:"verificationImage"`
	VerificationVolumeSize      string `json:"verificationVolumeSize"`
	Compression                 string `json:"compression"`
}

func credentialsSecretName(objectName string) string { return objectName + "-credentials" }

// --- Source CRUD ---

func (s *Server) handleAPICreateSource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "POST required")
		return
	}
	var req sourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid JSON")
		return
	}
	if req.Name == "" || req.DBType == "" || req.Host == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "name, dbType, host are required")
		return
	}
	// Redis can run with password-only AUTH (no username); every other DB
	// type still requires a username.
	if req.Username == "" && req.DBType != v1alpha1.DatabaseRedis {
		writeError(w, http.StatusBadRequest, codeBadRequest, "username is required for "+req.DBType)
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

	bs := &v1alpha1.BackupSource{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: s.cfg.Namespace,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "backup-operator-ui"},
		},
	}
	bs.Spec.Database.CredentialsSecretRef = v1alpha1.CredentialsSecretRef{Name: credentialsSecretName(req.Name)}
	if msg := applySourceRequest(&bs.Spec, req, false); msg != "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, msg)
		return
	}

	if err := s.cfg.Client.Create(r.Context(), bs); err != nil {
		s.cfg.Logger.Error(err, "create backupsource")
		writeError(w, http.StatusConflict, codeConflict, "failed to create: "+sanitizeError(err))
		return
	}
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            credentialsSecretName(req.Name),
			Namespace:       s.cfg.Namespace,
			Labels:          map[string]string{"app.kubernetes.io/managed-by": "backup-operator-ui"},
			OwnerReferences: []metav1.OwnerReference{ownerRefFor(bs, v1alpha1.KindBackupSource)},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{},
	}
	if req.Password != "" {
		creds.Data["password"] = []byte(req.Password)
	}
	if err := s.cfg.Client.Create(r.Context(), creds); err != nil {
		// Roll the CR back so the user does not end up with a source that
		// can never authenticate and a 409 on retry.
		_ = s.cfg.Client.Delete(r.Context(), bs)
		s.cfg.Logger.Error(err, "create source credentials secret")
		writeError(w, http.StatusConflict, codeConflict, "failed to create credentials: "+sanitizeError(err))
		return
	}
	s.emitObjectEvent(r.Context(), bs, v1alpha1.KindBackupSource, "SourceCreated", fmt.Sprintf("Source %q created via UI", req.Name))
	s.broadcast(sseEvent{Type: "source_created", Data: req.Name})
	writeJSON(w, http.StatusCreated, apiResponse{OK: true, Name: bs.Name})
}

func (s *Server) handleAPIUpdateSource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "PUT required")
		return
	}
	name := trimPrefixPath(r.URL.Path, "/api/sources/")
	if name == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "source name required")
		return
	}
	var req sourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid JSON")
		return
	}
	if req.DBType != "" && !isSupportedDBType(req.DBType) {
		writeError(w, http.StatusBadRequest, codeBadRequest, "dbType must be postgres, mysql, mariadb, mongo, or redis")
		return
	}

	bs := &v1alpha1.BackupSource{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: name}, bs); err != nil {
		writeError(w, http.StatusNotFound, codeNotFound, "source not found")
		return
	}
	if msg := applySourceRequest(&bs.Spec, req, true); msg != "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, msg)
		return
	}
	if err := s.cfg.Client.Update(r.Context(), bs); err != nil {
		s.cfg.Logger.Error(err, "update backupsource")
		writeError(w, http.StatusInternalServerError, codeInternal, "update failed")
		return
	}
	if req.Password != "" {
		if status, msg := s.writeSourcePassword(r, bs, req.Password); msg != "" {
			writeError(w, status, codeConflict, msg)
			return
		}
	}
	s.emitObjectEvent(r.Context(), bs, v1alpha1.KindBackupSource, "SourceUpdated", fmt.Sprintf("Source %q updated via UI", name))
	s.broadcast(sseEvent{Type: "source_updated", Data: name})
	writeJSON(w, http.StatusOK, apiResponse{OK: true, Name: name})
}

// writeSourcePassword stores a new password for the source. It only edits a
// credentials Secret the UI itself owns (OwnerReference back to the CR): a
// Secret the user manages — say the one CloudNativePG generated for the
// database — must never be overwritten from the backup dashboard. A missing
// Secret is created and owned; a foreign one is reported as a conflict.
func (s *Server) writeSourcePassword(r *http.Request, bs *v1alpha1.BackupSource, password string) (int, string) {
	ref := bs.Spec.Database.CredentialsSecretRef
	key := ref.PasswordKey
	if key == "" {
		key = "password"
	}
	creds := &corev1.Secret{}
	err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: bs.Namespace, Name: ref.Name}, creds)
	switch {
	case apierrors.IsNotFound(err):
		creds = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:            ref.Name,
				Namespace:       bs.Namespace,
				Labels:          map[string]string{"app.kubernetes.io/managed-by": "backup-operator-ui"},
				OwnerReferences: []metav1.OwnerReference{ownerRefFor(bs, v1alpha1.KindBackupSource)},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{key: []byte(password)},
		}
		if err := s.cfg.Client.Create(r.Context(), creds); err != nil {
			s.cfg.Logger.Error(err, "create source credentials secret")
			return http.StatusInternalServerError, "failed to store password"
		}
		return 0, ""
	case err != nil:
		s.cfg.Logger.Error(err, "get source credentials secret")
		return http.StatusInternalServerError, "failed to read credentials"
	}
	if !ownedBy(creds, bs.UID) {
		return http.StatusConflict, fmt.Sprintf("credentials Secret %q is not managed by the UI; update it directly", ref.Name)
	}
	if creds.Data == nil {
		creds.Data = map[string][]byte{}
	}
	creds.Data[key] = []byte(password)
	if err := s.cfg.Client.Update(r.Context(), creds); err != nil {
		s.cfg.Logger.Error(err, "update source credentials secret")
		return http.StatusInternalServerError, "failed to store password"
	}
	return 0, ""
}

func (s *Server) handleAPIDeleteSource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "DELETE required")
		return
	}
	name := trimPrefixPath(r.URL.Path, "/api/sources/")
	if name == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "source name required")
		return
	}
	bs := &v1alpha1.BackupSource{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: name}, bs); err != nil {
		writeError(w, http.StatusNotFound, codeNotFound, "not found or already deleted")
		return
	}
	// The CronJob and a UI-created credentials Secret both carry an
	// OwnerReference to the CR, so Kubernetes GC cascades; nothing else to do.
	if err := s.cfg.Client.Delete(r.Context(), bs); err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "delete failed")
		return
	}
	s.emitObjectEvent(r.Context(), bs, v1alpha1.KindBackupSource, "SourceDeleted", fmt.Sprintf("Source %q deleted via UI", name))
	s.broadcast(sseEvent{Type: "source_deleted", Data: name})
	writeJSON(w, http.StatusOK, apiResponse{OK: true})
}

// handleAPISuspendSource toggles spec.suspended. The reconciler observes the
// change and writes Spec.Suspend on the managed CronJob. Separate from the
// regular update endpoint so a one-click pause does not require sending the
// whole source body (and risk overwriting a concurrent edit).
func (s *Server) handleAPISuspendSource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "POST required")
		return
	}
	rest := trimPrefixPath(r.URL.Path, "/api/sources/")
	name := strings.TrimSuffix(rest, "/suspend")
	if name == "" || strings.Contains(name, "/") {
		writeError(w, http.StatusBadRequest, codeBadRequest, "source name required")
		return
	}
	var body struct {
		Suspend bool `json:"suspend"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid JSON")
		return
	}
	bs := &v1alpha1.BackupSource{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: name}, bs); err != nil {
		writeError(w, http.StatusNotFound, codeNotFound, "source not found")
		return
	}
	bs.Spec.Suspended = body.Suspend
	if err := s.cfg.Client.Update(r.Context(), bs); err != nil {
		s.cfg.Logger.Error(err, "update suspended")
		writeError(w, http.StatusInternalServerError, codeInternal, "update failed")
		return
	}
	evType, action := "source_resumed", "resumed"
	if body.Suspend {
		evType, action = "source_suspended", "suspended"
	}
	s.emitObjectEvent(r.Context(), bs, v1alpha1.KindBackupSource, "SourceUpdated", fmt.Sprintf("Source %q %s via UI", name, action))
	s.broadcast(sseEvent{Type: evType, Data: name})
	writeJSON(w, http.StatusOK, apiResponse{OK: true, Name: name})
}

// handleAPIGetSource returns a single source's configuration (never the
// password — only whether one is stored).
func (s *Server) handleAPIGetSource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "GET required")
		return
	}
	name := trimPrefixPath(r.URL.Path, "/api/sources/")
	if name == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "source name required")
		return
	}
	bs := &v1alpha1.BackupSource{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: name}, bs); err != nil {
		writeError(w, http.StatusNotFound, codeNotFound, "source not found")
		return
	}
	hasPassword := false
	creds := &corev1.Secret{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: bs.Spec.Database.CredentialsSecretRef.Name}, creds); err == nil {
		key := bs.Spec.Database.CredentialsSecretRef.PasswordKey
		if key == "" {
			key = "password"
		}
		hasPassword = len(creds.Data[key]) > 0
	}
	writeJSON(w, http.StatusOK, sourceInfoFrom(bs, hasPassword))
}

// sourceInfoFrom flattens the spec into the string-typed form shape.
func sourceInfoFrom(bs *v1alpha1.BackupSource, hasPassword bool) sourceInfo {
	spec := bs.Spec
	db := spec.Database
	info := sourceInfo{
		ObjectName:        bs.Name,
		Name:              bs.Name,
		TargetName:        spec.TargetName,
		DBType:            db.Type,
		Schedule:          spec.Schedule,
		Suspended:         spec.Suspended,
		Host:              db.Host,
		Database:          db.Name,
		Username:          db.Username,
		CredentialsSecret: db.CredentialsSecretRef.Name,
		HasPassword:       hasPassword,
		Destinations:      strings.Join(spec.Destinations, ","),
		Compression:       spec.Compression,
	}
	if spec.Compression == v1alpha1.CompressionGzip {
		// gzip is the schema default; the form shows "" for "default".
		info.Compression = ""
	}
	if db.Port != nil {
		info.Port = strconv.Itoa(int(*db.Port))
	}
	if spec.JitterMinutes != nil {
		info.JitterMinutes = strconv.Itoa(int(*spec.JitterMinutes))
	}
	if r := spec.Retention; r != nil {
		if r.Days != nil {
			info.RetentionDays = strconv.Itoa(int(*r.Days))
		}
		if r.MinKeep != nil {
			info.MinKeep = strconv.Itoa(int(*r.MinKeep))
		}
	}
	if a := spec.Analyzer; a != nil {
		if a.Enabled != nil {
			info.AnalyzerEnabled = strconv.FormatBool(*a.Enabled)
		}
		if a.EmptyDumpCheck != nil {
			info.EmptyDumpCheck = strconv.FormatBool(*a.EmptyDumpCheck)
		}
		if a.AnonymizeTables {
			info.AnonymizeTables = "true"
		}
		if a.RowDropThresholdPercent != nil {
			info.RowDropThreshold = percentToRatio(*a.RowDropThresholdPercent)
		}
		if a.SizeDropThresholdPercent != nil {
			info.SizeDropThreshold = percentToRatio(*a.SizeDropThresholdPercent)
		}
	}
	if rv := spec.RestoreVerification; rv != nil {
		if rv.Mode != v1alpha1.VerificationOff {
			info.RestoreVerificationMode = rv.Mode
		}
		if rv.Interval != nil {
			info.RestoreVerificationInterval = rv.Interval.Duration.String()
		}
		info.VerificationImage = rv.Image
		if rv.VolumeSize != nil {
			info.VerificationVolumeSize = rv.VolumeSize.String()
		}
	}
	return info
}

func applySourceRequest(spec *v1alpha1.BackupSourceSpec, req sourceRequest, update bool) string {
	for _, step := range []func() string{
		func() string { return applyDatabaseRequest(&spec.Database, req, update) },
		func() string { return applyScheduleRequest(spec, req, update) },
		func() string { return applyRetentionRequest(spec, req, update) },
		func() string { return applyAnalyzerRequest(spec, req, update) },
		func() string { return applyVerificationRequest(spec, req) },
	} {
		if msg := step(); msg != "" {
			return msg
		}
	}
	if len(req.Extra) > 0 {
		if spec.Database.Extra == nil {
			spec.Database.Extra = map[string]string{}
		}
		for k, v := range req.Extra {
			spec.Database.Extra[k] = v
		}
	}
	return ""
}

// applyDatabaseRequest sets the connection identity fields; empty values are
// preserved on update (host/type/database/username), except the port which
// the form always submits.
func applyDatabaseRequest(db *v1alpha1.DatabaseSpec, req sourceRequest, update bool) string {
	if req.DBType != "" {
		db.Type = req.DBType
	}
	if req.Host != "" {
		db.Host = req.Host
	}
	if req.Database != "" {
		db.Name = req.Database
	}
	if req.Username != "" {
		db.Username = req.Username
	}
	switch {
	case req.Port != "":
		if msg := validatePort(req.Port); msg != "" {
			return msg
		}
		p, _ := strconv.ParseInt(strings.TrimSpace(req.Port), 10, 32)
		db.Port = ptrInt32(int32(p))
	case update:
		db.Port = nil
	}
	return ""
}

// applyScheduleRequest sets schedule, jitter and the destination allow-list.
func applyScheduleRequest(spec *v1alpha1.BackupSourceSpec, req sourceRequest, update bool) string {
	if req.Schedule != "" {
		if msg := validateCronSchedule(req.Schedule); msg != "" {
			return msg
		}
		spec.Schedule = req.Schedule
	}
	switch {
	case req.JitterMinutes != "":
		n, err := strconv.ParseInt(strings.TrimSpace(req.JitterMinutes), 10, 32)
		if err != nil || n < 0 || n > 60 {
			return "jitterMinutes must be an integer between 0 and 60"
		}
		spec.JitterMinutes = ptrInt32(int32(n))
	case update:
		spec.JitterMinutes = nil
	}
	spec.Destinations = nil
	for _, d := range strings.Split(req.Destinations, ",") {
		if d = strings.TrimSpace(d); d != "" {
			spec.Destinations = append(spec.Destinations, d)
		}
	}
	return ""
}

// applyRetentionRequest sets retention days / minKeep and the compression
// algorithm; an empty retention clears the block on update.
func applyRetentionRequest(spec *v1alpha1.BackupSourceSpec, req sourceRequest, update bool) string {
	ret := &v1alpha1.RetentionSpec{}
	if req.RetentionDays != "" {
		n, err := strconv.ParseInt(strings.TrimSpace(req.RetentionDays), 10, 32)
		if err != nil || n < 0 {
			return "retentionDays must be a non-negative integer"
		}
		ret.Days = ptrInt32(int32(n))
	}
	if req.MinKeep != "" {
		n, err := strconv.ParseInt(strings.TrimSpace(req.MinKeep), 10, 32)
		if err != nil || n < 0 {
			return "minKeep must be a non-negative integer"
		}
		ret.MinKeep = ptrInt32(int32(n))
	}
	switch {
	case ret.Days != nil || ret.MinKeep != nil:
		spec.Retention = ret
	case update:
		spec.Retention = nil
	}
	switch c := strings.ToLower(strings.TrimSpace(req.Compression)); c {
	case "":
		if update {
			spec.Compression = ""
		}
	case v1alpha1.CompressionGzip, v1alpha1.CompressionZstd:
		spec.Compression = c
	default:
		return "compression must be gzip or zstd"
	}
	return ""
}

// applyAnalyzerRequest sets the analyzer toggles and converts the form's
// 0..1 ratios to whole percents; an all-default block is dropped.
func applyAnalyzerRequest(spec *v1alpha1.BackupSourceSpec, req sourceRequest, update bool) string {
	an := &v1alpha1.AnalyzerSpec{}
	if spec.Analyzer != nil && !update {
		an = spec.Analyzer
	}
	an.Enabled = req.AnalyzerEnabled
	an.EmptyDumpCheck = req.EmptyDumpCheck
	an.AnonymizeTables = req.AnonymizeTables != nil && *req.AnonymizeTables
	if req.RowDropThreshold != "" {
		p, msg := ratioToPercent(req.RowDropThreshold, "rowDropThreshold")
		if msg != "" {
			return msg
		}
		an.RowDropThresholdPercent = p
	}
	if req.SizeDropThreshold != "" {
		p, msg := ratioToPercent(req.SizeDropThreshold, "sizeDropThreshold")
		if msg != "" {
			return msg
		}
		an.SizeDropThresholdPercent = p
	}
	if an.Enabled != nil || an.EmptyDumpCheck != nil || an.AnonymizeTables || an.RowDropThresholdPercent != nil || an.SizeDropThresholdPercent != nil {
		spec.Analyzer = an
	} else {
		spec.Analyzer = nil
	}
	return ""
}

// applyVerificationRequest sets the restore-verification block; an all-off
// block is dropped so the spec stays minimal.
func applyVerificationRequest(spec *v1alpha1.BackupSourceSpec, req sourceRequest) string {
	rv := &v1alpha1.RestoreVerificationSpec{}
	switch m := strings.ToLower(strings.TrimSpace(req.RestoreVerificationMode)); m {
	case "", v1alpha1.VerificationOff:
		rv.Mode = v1alpha1.VerificationOff
	case v1alpha1.VerificationStreamValidate, v1alpha1.VerificationSchemaOnly, v1alpha1.VerificationSample, v1alpha1.VerificationFull:
		rv.Mode = m
	default:
		return "restoreVerificationMode must be off, stream-validate, schema-only, sample, or full"
	}
	if req.RestoreVerificationInterval != "" {
		d, err := time.ParseDuration(strings.TrimSpace(req.RestoreVerificationInterval))
		if err != nil || d <= 0 {
			return "restoreVerificationInterval must be a positive Go duration (e.g. 168h)"
		}
		rv.Interval = &metav1.Duration{Duration: d}
	}
	rv.Image = strings.TrimSpace(req.VerificationImage)
	if req.VerificationVolumeSize != "" {
		q, err := resource.ParseQuantity(strings.TrimSpace(req.VerificationVolumeSize))
		if err != nil {
			return "verificationVolumeSize must be a Kubernetes quantity (e.g. 50Gi)"
		}
		rv.VolumeSize = &q
	}
	if rv.Mode != v1alpha1.VerificationOff || rv.Interval != nil || rv.Image != "" || rv.VolumeSize != nil {
		spec.RestoreVerification = rv
	} else {
		spec.RestoreVerification = nil
	}
	return ""
}

// ratioToPercent converts the form's legacy 0..1 ratio ("0.3") to the
// spec's whole percent (30).
func ratioToPercent(v, field string) (*int32, string) {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || f <= 0 || f > 1 {
		return nil, field + " must be a ratio between 0 and 1 (e.g. 0.5)"
	}
	return ptrInt32(int32(f*100 + 0.5)), ""
}

func percentToRatio(p int32) string {
	return strconv.FormatFloat(float64(p)/100, 'f', -1, 64)
}

func ptrInt32(n int32) *int32 { return &n }

// ownerRefFor builds a non-controller OwnerReference so Kubernetes GC
// removes the credentials Secret with its CR.
func ownerRefFor(obj client.Object, kind string) metav1.OwnerReference {
	t := true
	return metav1.OwnerReference{
		APIVersion:         v1alpha1.GroupVersion.String(),
		Kind:               kind,
		Name:               obj.GetName(),
		UID:                obj.GetUID(),
		BlockOwnerDeletion: &t,
	}
}

// ownedBy reports whether obj carries an OwnerReference to the given UID.
func ownedBy(obj client.Object, uid types.UID) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == uid {
			return true
		}
	}
	return false
}
