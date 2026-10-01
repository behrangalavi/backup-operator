package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"backup-operator/api/v1alpha1"
	"backup-operator/internal/secrets"
)

// destinationRequest is the JSON body for POST /api/destinations and PUT
// /api/destinations/{name}. Data carries the flat key/value form the SPA
// has always sent (host, port, bucket, password, ...); the handler routes
// connection keys into the BackupDestination spec and credential keys into
// the credentials Secret.
type destinationRequest struct {
	Name        string            `json:"name"`
	StorageType string            `json:"storageType"`
	PathPrefix  string            `json:"pathPrefix"`
	Data        map[string]string `json:"data"`
	// RemoveKeys lists keys the caller wants explicitly cleared on update.
	// PUT merges, so a missing key preserves the stored value; this is the
	// escape hatch for "switch SFTP auth from key to password".
	RemoveKeys []string `json:"removeKeys,omitempty"`
}

type destInfo struct {
	ObjectName        string            `json:"objectName"`
	Name              string            `json:"name"`
	StorageType       string            `json:"storageType"`
	PathPrefix        string            `json:"pathPrefix"`
	Host              string            `json:"host"`
	CredentialsSecret string            `json:"credentialsSecret,omitempty"`
	CreatedAt         string            `json:"createdAt,omitempty"`
	Data              map[string]string `json:"data,omitempty"`
}

// connectionKeys are the flat form keys that live in the spec, per type.
// Everything in secrets.CredentialKeys goes to the Secret instead.
var connectionKeys = map[string][]string{
	v1alpha1.StorageSFTP:  {"host", "port", "username", "known-hosts", "insecure-skip-host-verify"},
	v1alpha1.StorageFTPS:  {"host", "port", "username", "tls-mode", "insecure-skip-cert-verify"},
	v1alpha1.StorageS3:    {"bucket", "endpoint", "region", "path-style"},
	v1alpha1.StorageAzure: {"account-name", "container"},
	v1alpha1.StorageGCS:   {"bucket"},
}

// normalizeStorageType folds the legacy hetzner-sftp alias (still offered
// by the form because it preselects port 23) into sftp.
func normalizeStorageType(t string) (string, bool) {
	switch t {
	case "hetzner-sftp":
		return v1alpha1.StorageSFTP, true
	case v1alpha1.StorageSFTP, v1alpha1.StorageFTPS, v1alpha1.StorageS3, v1alpha1.StorageAzure, v1alpha1.StorageGCS:
		return t, false
	}
	return "", false
}

func (s *Server) handleAPIListDestinations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "GET required")
		return
	}
	var list v1alpha1.BackupDestinationList
	if err := s.cfg.Client.List(r.Context(), &list, client.InNamespace(s.cfg.Namespace)); err != nil {
		s.cfg.Logger.Error(err, "list backupdestinations")
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	out := make([]destInfo, 0, len(list.Items))
	for i := range list.Items {
		bd := &list.Items[i]
		info := destInfo{
			ObjectName:        bd.Name,
			Name:              bd.Name,
			StorageType:       bd.Spec.Type,
			PathPrefix:        bd.Spec.PathPrefix,
			Host:              destinationHost(&bd.Spec),
			CredentialsSecret: bd.Spec.CredentialsSecretRef.Name,
		}
		if !bd.CreationTimestamp.IsZero() {
			info.CreatedAt = bd.CreationTimestamp.UTC().Format(time.RFC3339)
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, out)
}

// destinationHost picks the one connection field worth showing in a list row.
func destinationHost(spec *v1alpha1.BackupDestinationSpec) string {
	switch {
	case spec.SFTP != nil:
		return spec.SFTP.Host
	case spec.FTPS != nil:
		return spec.FTPS.Host
	case spec.S3 != nil:
		if spec.S3.Endpoint != "" {
			return spec.S3.Endpoint
		}
		return spec.S3.Bucket
	case spec.Azure != nil:
		return spec.Azure.AccountName
	case spec.GCS != nil:
		return spec.GCS.Bucket
	}
	return ""
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
	storageType, hetzner := normalizeStorageType(req.StorageType)
	if storageType == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "storageType must be sftp, hetzner-sftp, ftps, s3, azure, or gcs")
		return
	}
	if msg := validateK8sName(req.Name); msg != "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, msg)
		return
	}

	bd := &v1alpha1.BackupDestination{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: s.cfg.Namespace,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "backup-operator-ui"},
		},
		Spec: v1alpha1.BackupDestinationSpec{
			Type:                 storageType,
			PathPrefix:           req.PathPrefix,
			CredentialsSecretRef: v1alpha1.LocalSecretRef{Name: credentialsSecretName(req.Name)},
		},
	}
	credData := map[string][]byte{}
	if hetzner {
		if _, ok := req.Data["port"]; !ok {
			req.Data["port"] = "23"
		}
	}
	if msg := applyDestinationData(&bd.Spec, req.Data, nil, credData); msg != "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, msg)
		return
	}
	if _, err := secrets.FromBackupDestination(bd, nil); err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid destination: "+err.Error())
		return
	}
	if err := s.cfg.Client.Create(r.Context(), bd); err != nil {
		s.cfg.Logger.Error(err, "create backupdestination")
		writeError(w, http.StatusConflict, codeConflict, "failed to create: "+sanitizeError(err))
		return
	}
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            bd.Spec.CredentialsSecretRef.Name,
			Namespace:       s.cfg.Namespace,
			Labels:          map[string]string{"app.kubernetes.io/managed-by": "backup-operator-ui"},
			OwnerReferences: []metav1.OwnerReference{ownerRefFor(bd, v1alpha1.KindBackupDestination)},
		},
		Type: corev1.SecretTypeOpaque,
		Data: credData,
	}
	if err := s.cfg.Client.Create(r.Context(), creds); err != nil {
		_ = s.cfg.Client.Delete(r.Context(), bd)
		s.cfg.Logger.Error(err, "create destination credentials secret")
		writeError(w, http.StatusConflict, codeConflict, "failed to create credentials: "+sanitizeError(err))
		return
	}
	s.emitObjectEvent(r.Context(), bd, v1alpha1.KindBackupDestination, "DestinationCreated", fmt.Sprintf("Destination %q created via UI", req.Name))
	s.broadcast(sseEvent{Type: "destination_created", Data: req.Name})
	writeJSON(w, http.StatusCreated, apiResponse{OK: true, Name: bd.Name})
}

func (s *Server) handleAPIUpdateDestination(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "PUT required")
		return
	}
	name := trimPrefixPath(r.URL.Path, "/api/destinations/")
	if name == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "destination name required")
		return
	}
	var req destinationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid JSON")
		return
	}
	bd := &v1alpha1.BackupDestination{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: name}, bd); err != nil {
		writeError(w, http.StatusNotFound, codeNotFound, "destination not found")
		return
	}
	if req.StorageType != "" {
		storageType, _ := normalizeStorageType(req.StorageType)
		if storageType == "" {
			writeError(w, http.StatusBadRequest, codeBadRequest, "unsupported storageType")
			return
		}
		if storageType != bd.Spec.Type {
			// Switching backend means a different spec block and different
			// credential keys; start from a clean spec of the new type.
			bd.Spec.SFTP, bd.Spec.FTPS, bd.Spec.S3, bd.Spec.Azure, bd.Spec.GCS = nil, nil, nil, nil, nil
			bd.Spec.Type = storageType
		}
	}
	// PathPrefix: the form always sends it, so "" means the user cleared it.
	bd.Spec.PathPrefix = req.PathPrefix

	credSet := map[string][]byte{}
	credRemove := map[string]bool{}
	if msg := applyDestinationData(&bd.Spec, req.Data, req.RemoveKeys, credSet); msg != "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, msg)
		return
	}
	for _, k := range req.RemoveKeys {
		if isCredentialKey(bd.Spec.Type, k) {
			credRemove[k] = true
		}
	}
	if _, err := secrets.FromBackupDestination(bd, nil); err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid destination: "+err.Error())
		return
	}
	if err := s.cfg.Client.Update(r.Context(), bd); err != nil {
		s.cfg.Logger.Error(err, "update backupdestination")
		writeError(w, http.StatusInternalServerError, codeInternal, "update failed")
		return
	}
	if len(credSet) > 0 || len(credRemove) > 0 {
		if status, msg := s.writeDestinationCredentials(r.Context(), bd, credSet, credRemove); msg != "" {
			writeError(w, status, codeConflict, msg)
			return
		}
	}
	s.emitObjectEvent(r.Context(), bd, v1alpha1.KindBackupDestination, "DestinationUpdated", fmt.Sprintf("Destination %q updated via UI", name))
	s.broadcast(sseEvent{Type: "destination_updated", Data: name})
	writeJSON(w, http.StatusOK, apiResponse{OK: true, Name: name})
}

// writeDestinationCredentials mirrors writeSourcePassword: only a Secret
// the UI owns is edited in place; a missing one is created and owned; a
// foreign one is a conflict.
func (s *Server) writeDestinationCredentials(ctx context.Context, bd *v1alpha1.BackupDestination, set map[string][]byte, remove map[string]bool) (int, string) {
	name := bd.Spec.CredentialsSecretRef.Name
	creds := &corev1.Secret{}
	err := s.cfg.Client.Get(ctx, client.ObjectKey{Namespace: bd.Namespace, Name: name}, creds)
	switch {
	case apierrors.IsNotFound(err):
		creds = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       bd.Namespace,
				Labels:          map[string]string{"app.kubernetes.io/managed-by": "backup-operator-ui"},
				OwnerReferences: []metav1.OwnerReference{ownerRefFor(bd, v1alpha1.KindBackupDestination)},
			},
			Type: corev1.SecretTypeOpaque,
			Data: set,
		}
		if err := s.cfg.Client.Create(ctx, creds); err != nil {
			s.cfg.Logger.Error(err, "create destination credentials secret")
			return http.StatusInternalServerError, "failed to store credentials"
		}
		return 0, ""
	case err != nil:
		s.cfg.Logger.Error(err, "get destination credentials secret")
		return http.StatusInternalServerError, "failed to read credentials"
	}
	if !ownedBy(creds, bd.UID) {
		return http.StatusConflict, fmt.Sprintf("credentials Secret %q is not managed by the UI; update it directly", name)
	}
	if creds.Data == nil {
		creds.Data = map[string][]byte{}
	}
	for k, v := range set {
		creds.Data[k] = v
	}
	for k := range remove {
		delete(creds.Data, k)
	}
	if err := s.cfg.Client.Update(ctx, creds); err != nil {
		s.cfg.Logger.Error(err, "update destination credentials secret")
		return http.StatusInternalServerError, "failed to store credentials"
	}
	return 0, ""
}

func (s *Server) handleAPIDeleteDestination(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "DELETE required")
		return
	}
	name := trimPrefixPath(r.URL.Path, "/api/destinations/")
	if name == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "destination name required")
		return
	}
	bd := &v1alpha1.BackupDestination{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: name}, bd); err != nil {
		writeError(w, http.StatusNotFound, codeNotFound, "not found or already deleted")
		return
	}
	if err := s.cfg.Client.Delete(r.Context(), bd); err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "delete failed")
		return
	}
	s.emitObjectEvent(r.Context(), bd, v1alpha1.KindBackupDestination, "DestinationDeleted", fmt.Sprintf("Destination %q deleted via UI", name))
	s.broadcast(sseEvent{Type: "destination_deleted", Data: name})
	writeJSON(w, http.StatusOK, apiResponse{OK: true})
}

// handleAPIGetDestination returns the destination's configuration. The
// connection fields come back verbatim in the flat data map; every
// credential key present in the Secret comes back as "***" so the edit form
// knows which auth method is stored without ever seeing the value.
func (s *Server) handleAPIGetDestination(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "GET required")
		return
	}
	name := trimPrefixPath(r.URL.Path, "/api/destinations/")
	if name == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "destination name required")
		return
	}
	bd := &v1alpha1.BackupDestination{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: name}, bd); err != nil {
		writeError(w, http.StatusNotFound, codeNotFound, "destination not found")
		return
	}
	data := flattenDestinationSpec(&bd.Spec)
	creds := &corev1.Secret{}
	if err := s.cfg.Client.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.Namespace, Name: bd.Spec.CredentialsSecretRef.Name}, creds); err == nil {
		for _, k := range secrets.CredentialKeys(bd.Spec.Type) {
			if len(creds.Data[k]) > 0 {
				data[k] = "***"
			}
		}
	}
	writeJSON(w, http.StatusOK, destInfo{
		ObjectName:        bd.Name,
		Name:              bd.Name,
		StorageType:       bd.Spec.Type,
		PathPrefix:        bd.Spec.PathPrefix,
		Host:              destinationHost(&bd.Spec),
		CredentialsSecret: bd.Spec.CredentialsSecretRef.Name,
		Data:              data,
	})
}

// flattenDestinationSpec renders the typed spec block back into the flat
// key/value form the SPA edits.
func flattenDestinationSpec(spec *v1alpha1.BackupDestinationSpec) map[string]string {
	out := map[string]string{}
	port := func(p *int32) {
		if p != nil {
			out["port"] = strconv.Itoa(int(*p))
		}
	}
	switch {
	case spec.SFTP != nil:
		out["host"] = spec.SFTP.Host
		port(spec.SFTP.Port)
		out["username"] = spec.SFTP.Username
		if spec.SFTP.KnownHosts != "" {
			out["known-hosts"] = spec.SFTP.KnownHosts
		}
		if spec.SFTP.InsecureSkipHostVerify {
			out["insecure-skip-host-verify"] = "true"
		}
	case spec.FTPS != nil:
		out["host"] = spec.FTPS.Host
		port(spec.FTPS.Port)
		out["username"] = spec.FTPS.Username
		if spec.FTPS.TLSMode != "" {
			out["tls-mode"] = spec.FTPS.TLSMode
		}
		if spec.FTPS.InsecureSkipCertVerify {
			out["insecure-skip-cert-verify"] = "true"
		}
	case spec.S3 != nil:
		out["bucket"] = spec.S3.Bucket
		if spec.S3.Endpoint != "" {
			out["endpoint"] = spec.S3.Endpoint
		}
		if spec.S3.Region != "" {
			out["region"] = spec.S3.Region
		}
		if spec.S3.PathStyle {
			out["path-style"] = "true"
		}
	case spec.Azure != nil:
		out["account-name"] = spec.Azure.AccountName
		out["container"] = spec.Azure.Container
	case spec.GCS != nil:
		out["bucket"] = spec.GCS.Bucket
	}
	return out
}

func isCredentialKey(storageType, key string) bool {
	for _, k := range secrets.CredentialKeys(storageType) {
		if k == key {
			return true
		}
	}
	return false
}

// applyDestinationData routes the flat form data into the spec (connection
// keys) and the credentials map (credential keys). removeKeys clear
// connection fields; credential removals are handled by the caller
// because they touch the Secret. Unknown keys are rejected so a typo can't
// silently vanish. Returns a user-facing message, or "".
func applyDestinationData(spec *v1alpha1.BackupDestinationSpec, data map[string]string, removeKeys []string, creds map[string][]byte) string {
	conn := map[string]bool{}
	for _, k := range connectionKeys[spec.Type] {
		conn[k] = true
	}
	flat := map[string]*string{} // nil = remove
	for _, k := range removeKeys {
		if conn[k] {
			flat[k] = nil
		}
	}
	for k, v := range data {
		v := v
		switch {
		case conn[k]:
			flat[k] = &v
		case isCredentialKey(spec.Type, k):
			if v != "" {
				creds[k] = []byte(v)
			}
		default:
			return fmt.Sprintf("unknown field %q for storage type %s", k, spec.Type)
		}
	}
	get := func(k string, cur string) string {
		if p, ok := flat[k]; ok {
			if p == nil {
				return ""
			}
			return strings.TrimSpace(*p)
		}
		return cur
	}
	getPort := func(cur *int32) (*int32, string) {
		p, ok := flat["port"]
		if !ok {
			return cur, ""
		}
		if p == nil || strings.TrimSpace(*p) == "" {
			return nil, ""
		}
		if msg := validatePort(*p); msg != "" {
			return nil, msg
		}
		n, _ := strconv.ParseInt(strings.TrimSpace(*p), 10, 32)
		return ptrInt32(int32(n)), ""
	}
	getBool := func(k string, cur bool) bool {
		if p, ok := flat[k]; ok {
			return p != nil && strings.EqualFold(strings.TrimSpace(*p), "true")
		}
		return cur
	}

	switch spec.Type {
	case v1alpha1.StorageSFTP:
		if spec.SFTP == nil {
			spec.SFTP = &v1alpha1.SFTPSpec{}
		}
		b := spec.SFTP
		b.Host = get("host", b.Host)
		b.Username = get("username", b.Username)
		b.KnownHosts = get("known-hosts", b.KnownHosts)
		b.InsecureSkipHostVerify = getBool("insecure-skip-host-verify", b.InsecureSkipHostVerify)
		p, msg := getPort(b.Port)
		if msg != "" {
			return msg
		}
		b.Port = p
	case v1alpha1.StorageFTPS:
		if spec.FTPS == nil {
			spec.FTPS = &v1alpha1.FTPSSpec{}
		}
		b := spec.FTPS
		b.Host = get("host", b.Host)
		b.Username = get("username", b.Username)
		b.TLSMode = get("tls-mode", b.TLSMode)
		b.InsecureSkipCertVerify = getBool("insecure-skip-cert-verify", b.InsecureSkipCertVerify)
		p, msg := getPort(b.Port)
		if msg != "" {
			return msg
		}
		b.Port = p
	case v1alpha1.StorageS3:
		if spec.S3 == nil {
			spec.S3 = &v1alpha1.S3Spec{}
		}
		b := spec.S3
		b.Bucket = get("bucket", b.Bucket)
		b.Endpoint = get("endpoint", b.Endpoint)
		b.Region = get("region", b.Region)
		b.PathStyle = getBool("path-style", b.PathStyle)
	case v1alpha1.StorageAzure:
		if spec.Azure == nil {
			spec.Azure = &v1alpha1.AzureSpec{}
		}
		b := spec.Azure
		b.AccountName = get("account-name", b.AccountName)
		b.Container = get("container", b.Container)
	case v1alpha1.StorageGCS:
		if spec.GCS == nil {
			spec.GCS = &v1alpha1.GCSSpec{}
		}
		spec.GCS.Bucket = get("bucket", spec.GCS.Bucket)
	default:
		return "unsupported storage type"
	}
	return ""
}

// --- Destination connectivity test ---

func (s *Server) handleAPITestDestination(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "POST required")
		return
	}
	name := trimPrefixPath(r.URL.Path, "/api/destinations/")
	name = strings.TrimSuffix(name, "/test")

	dest, err := s.data.getDestination(r.Context(), name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, codeNotFound, "destination not found")
			return
		}
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid destination config: "+sanitizeStorageError(err))
		return
	}

	st, err := s.storageFor(dest, "test-connection")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    false,
			"error": "storage init failed: " + sanitizeStorageError(err),
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
			"error": "upload failed: " + sanitizeStorageError(err),
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
			"error": "readback failed: " + sanitizeStorageError(err),
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
