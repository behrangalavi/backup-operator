package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"backup-operator/api/v1alpha1"
	"backup-operator/internal/agekeys"

	"filippo.io/age"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ageKeyView is the JSON shape returned to the UI for a single recipient.
// Hash is the first 12 hex chars of SHA256(recipient) — short enough to
// display, long enough to disambiguate between recipients.
type ageKeyView struct {
	Name      string `json:"name"`
	Recipient string `json:"recipient"`
	Hash      string `json:"hash"`
}

type ageKeysResponse struct {
	OK           bool         `json:"ok"`
	Message      string       `json:"message,omitempty"`
	Keys         []ageKeyView `json:"keys,omitempty"`
	CanMutate    bool         `json:"canMutate"`
	MergedSecret string       `json:"mergedSecret,omitempty"`
}

type ageKeyAddRequest struct {
	Recipient string `json:"recipient"`
}

// routeAgeKeys multiplexes /api/age-keys by HTTP method.
// GET is always allowed (read-only listing helps operators audit which
// recipients are configured). POST/DELETE require both ReadOnly=false
// AND AllowKeyMutation=true — opt-in defense in depth on top of the
// (optional) auth proxy.
func (s *Server) routeAgeKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListAgeKeys(w, r)
	case http.MethodPost:
		if !s.keyMutationAllowed(w) {
			return
		}
		s.handleAddAgeKey(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, ageKeysResponse{Message: "method not allowed"})
	}
}

// routeAgeKeyByRecipient handles DELETE /api/age-keys/<recipient-or-hash>.
func (s *Server) routeAgeKeyByRecipient(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, ageKeysResponse{Message: "DELETE required"})
		return
	}
	if !s.keyMutationAllowed(w) {
		return
	}
	recipient := trimPrefixPath(r.URL.Path, "/api/age-keys/")
	if recipient == "" {
		writeJSON(w, http.StatusBadRequest, ageKeysResponse{Message: "recipient required in path"})
		return
	}
	s.handleDeleteAgeKey(w, r, recipient)
}

func (s *Server) keyMutationAllowed(w http.ResponseWriter) bool {
	if s.cfg.ReadOnly {
		writeJSON(w, http.StatusForbidden, ageKeysResponse{Message: "UI is read-only"})
		return false
	}
	if !s.cfg.AllowKeyMutation {
		writeJSON(w, http.StatusForbidden, ageKeysResponse{Message: "age key mutation is disabled (set UI_ALLOW_KEY_MUTATION=true to enable)"})
		return false
	}
	return true
}

func (s *Server) handleListAgeKeys(w http.ResponseWriter, r *http.Request) {
	recs, err := s.listRecipients(r.Context())
	if err != nil {
		s.cfg.Logger.Error(err, "list backuprecipients")
		writeJSON(w, http.StatusInternalServerError, ageKeysResponse{Message: "failed to list age recipients"})
		return
	}
	views := make([]ageKeyView, 0, len(recs))
	seen := map[string]bool{}
	for i := range recs {
		key := strings.TrimSpace(recs[i].Spec.PublicKey)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		views = append(views, ageKeyView{Name: recs[i].Name, Recipient: key, Hash: agekeys.Hash(key)})
	}
	writeJSON(w, http.StatusOK, ageKeysResponse{
		OK:           true,
		Keys:         views,
		CanMutate:    !s.cfg.ReadOnly && s.cfg.AllowKeyMutation,
		MergedSecret: s.cfg.AgeSecretName,
	})
}

func (s *Server) handleAddAgeKey(w http.ResponseWriter, r *http.Request) {
	var req ageKeyAddRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ageKeysResponse{Message: "invalid JSON"})
		return
	}
	candidate := strings.TrimSpace(req.Recipient)
	if candidate == "" {
		writeJSON(w, http.StatusBadRequest, ageKeysResponse{Message: "recipient required"})
		return
	}
	// Authoritative parse — if age can't parse it as a recipient, the
	// worker can't either, and we'd silently break encryption. The crypto
	// package only loads X25519 recipients today (see crypto/age.go), so
	// we mirror that constraint here (and the CRD pattern does too).
	if _, err := age.ParseX25519Recipient(candidate); err != nil {
		writeJSON(w, http.StatusBadRequest, ageKeysResponse{Message: "invalid age recipient: " + err.Error()})
		return
	}
	existing, err := s.listRecipients(r.Context())
	if err != nil {
		s.cfg.Logger.Error(err, "list backuprecipients for add")
		writeJSON(w, http.StatusInternalServerError, ageKeysResponse{Message: "failed to read recipients"})
		return
	}
	for i := range existing {
		if strings.TrimSpace(existing[i].Spec.PublicKey) == candidate {
			writeJSON(w, http.StatusConflict, ageKeysResponse{Message: "recipient already present"})
			return
		}
	}

	rec := &v1alpha1.BackupRecipient{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agekeys.ObjectName(candidate),
			Namespace: s.cfg.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "backup-operator-ui",
				"app.kubernetes.io/component":  "age-recipient",
			},
		},
		Spec: v1alpha1.BackupRecipientSpec{PublicKey: candidate},
	}
	if err := s.cfg.Client.Create(r.Context(), rec); err != nil {
		if apierrors.IsAlreadyExists(err) {
			writeJSON(w, http.StatusConflict, ageKeysResponse{Message: "recipient already present"})
			return
		}
		s.cfg.Logger.Error(err, "create backuprecipient")
		writeJSON(w, http.StatusInternalServerError, ageKeysResponse{Message: "failed to save recipient"})
		return
	}
	s.emitObjectEvent(r.Context(), rec, v1alpha1.KindBackupRecipient, "AgeKeyAdded",
		fmt.Sprintf("Public key %s added via UI (%d total)", agekeys.Hash(candidate), len(existing)+1))
	s.broadcast(sseEvent{Type: "age_keys_updated", Data: "added"})
	writeJSON(w, http.StatusCreated, ageKeysResponse{OK: true, Message: "key added"})
}

func (s *Server) handleDeleteAgeKey(w http.ResponseWriter, r *http.Request, target string) {
	existing, err := s.listRecipients(r.Context())
	if err != nil {
		s.cfg.Logger.Error(err, "list backuprecipients for delete")
		writeJSON(w, http.StatusInternalServerError, ageKeysResponse{Message: "failed to read recipients"})
		return
	}
	// Match by exact recipient string, by object name, OR by hash prefix —
	// the UI sends the full recipient, but allowing hash makes scripted use
	// safer against pasting issues with long base32 strings.
	var match *v1alpha1.BackupRecipient
	nonEmpty := 0
	for i := range existing {
		rec := strings.TrimSpace(existing[i].Spec.PublicKey)
		if rec == "" {
			continue
		}
		nonEmpty++
		if match == nil && (rec == target || existing[i].Name == target || strings.HasPrefix(agekeys.Hash(rec), target)) {
			match = &existing[i]
		}
	}
	if match == nil {
		writeJSON(w, http.StatusNotFound, ageKeysResponse{Message: "recipient not found"})
		return
	}
	// Hard refusal: removing the last recipient leaves zero recipients,
	// which makes the worker fail to start (no encryption possible).
	// This is a destruction of capability, not a configuration choice.
	if nonEmpty <= 1 {
		s.emitObjectEvent(r.Context(), match, v1alpha1.KindBackupRecipient, "AgeKeyRemovalRefused",
			"Refused to remove the last age recipient — would disable encryption entirely")
		writeJSON(w, http.StatusConflict, ageKeysResponse{Message: "cannot remove the last recipient — at least one is required for encryption"})
		return
	}
	removed := strings.TrimSpace(match.Spec.PublicKey)
	if err := s.cfg.Client.Delete(r.Context(), match); err != nil {
		if apierrors.IsNotFound(err) {
			writeJSON(w, http.StatusOK, ageKeysResponse{OK: true, Message: "key removed"})
			return
		}
		s.cfg.Logger.Error(err, "delete backuprecipient")
		writeJSON(w, http.StatusInternalServerError, ageKeysResponse{Message: "failed to delete recipient"})
		return
	}
	s.emitObjectEvent(r.Context(), match, v1alpha1.KindBackupRecipient, "AgeKeyRemoved",
		fmt.Sprintf("Public key %s removed via UI (%d remaining)", agekeys.Hash(removed), nonEmpty-1))
	s.broadcast(sseEvent{Type: "age_keys_updated", Data: "removed"})
	writeJSON(w, http.StatusOK, ageKeysResponse{OK: true, Message: "key removed"})
}

// listRecipients returns every BackupRecipient in scope. Order is whatever
// the API server returns; callers that need stability should sort.
func (s *Server) listRecipients(ctx context.Context) ([]v1alpha1.BackupRecipient, error) {
	var list v1alpha1.BackupRecipientList
	var opts []client.ListOption
	if s.cfg.Namespace != "" {
		opts = append(opts, client.InNamespace(s.cfg.Namespace))
	}
	if err := s.cfg.Client.List(ctx, &list, opts...); err != nil {
		return nil, err
	}
	return list.Items, nil
}
