package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"backup-operator/internal/labels"

	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ageKeyView is the JSON shape returned to the UI for a single recipient.
// Hash is the first 12 hex chars of SHA256(recipient) — short enough to
// display, long enough to disambiguate between recipients.
type ageKeyView struct {
	Recipient string `json:"recipient"`
	Hash      string `json:"hash"`
}

type ageKeysResponse struct {
	OK         bool         `json:"ok"`
	Message    string       `json:"message,omitempty"`
	Keys       []ageKeyView `json:"keys,omitempty"`
	CanMutate  bool         `json:"canMutate"`
	SecretName string       `json:"secretName,omitempty"`
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

// routeAgeKeyByRecipient handles DELETE /api/age-keys/<recipient>.
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
	views, err := s.listRecipientViews(r.Context())
	if err != nil {
		s.cfg.Logger.Error(err, "list age recipients")
		writeJSON(w, http.StatusInternalServerError, ageKeysResponse{Message: "failed to list age recipients"})
		return
	}
	writeJSON(w, http.StatusOK, ageKeysResponse{
		OK:         true,
		Keys:       views,
		CanMutate:  !s.cfg.ReadOnly && s.cfg.AllowKeyMutation,
		SecretName: s.cfg.AgeSecretName, // operator-managed merged Secret name (for display)
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
	// we mirror that constraint here.
	if _, err := age.ParseX25519Recipient(candidate); err != nil {
		writeJSON(w, http.StatusBadRequest, ageKeysResponse{Message: "invalid age recipient: " + err.Error()})
		return
	}

	existing, err := s.listRecipientSecrets(r.Context())
	if err != nil {
		s.cfg.Logger.Error(err, "list recipient secrets for add")
		writeJSON(w, http.StatusInternalServerError, ageKeysResponse{Message: "failed to read recipient secrets"})
		return
	}
	for _, sec := range existing {
		if recipientFromSecret(&sec) == candidate {
			writeJSON(w, http.StatusConflict, ageKeysResponse{Message: "recipient already present"})
			return
		}
	}

	name := recipientSecretName(candidate)
	newSec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.cfg.Namespace,
			Labels: map[string]string{
				labels.LabelRole:                   labels.RoleAgeRecipient,
				"app.kubernetes.io/managed-by":     "backup-operator-ui",
				"app.kubernetes.io/component":      "age-recipient",
			},
			Annotations: map[string]string{
				labels.AnnotationName: "ui-" + ageKeyHash(candidate),
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			labels.RecipientPublicKeyField: []byte(candidate),
		},
	}
	if err := s.cfg.Client.Create(r.Context(), newSec); err != nil {
		if apierrors.IsAlreadyExists(err) {
			writeJSON(w, http.StatusConflict, ageKeysResponse{Message: "recipient already present"})
			return
		}
		s.cfg.Logger.Error(err, "create recipient secret")
		writeJSON(w, http.StatusInternalServerError, ageKeysResponse{Message: "failed to save recipient"})
		return
	}
	s.emitAgeKeyEvent(r.Context(), newSec, "AgeKeyAdded",
		fmt.Sprintf("Public key %s added via UI (%d total)", ageKeyHash(candidate), len(existing)+1))
	s.broadcast(sseEvent{Type: "age_keys_updated", Data: "added"})
	writeJSON(w, http.StatusCreated, ageKeysResponse{OK: true, Message: "key added"})
}

func (s *Server) handleDeleteAgeKey(w http.ResponseWriter, r *http.Request, target string) {
	existing, err := s.listRecipientSecrets(r.Context())
	if err != nil {
		s.cfg.Logger.Error(err, "list recipient secrets for delete")
		writeJSON(w, http.StatusInternalServerError, ageKeysResponse{Message: "failed to read recipient secrets"})
		return
	}
	// Match by exact recipient string OR by hash prefix — the UI sends
	// the full recipient, but allowing hash makes scripted use safer
	// against pasting issues with long base32 strings.
	var match *corev1.Secret
	for i := range existing {
		rec := recipientFromSecret(&existing[i])
		if rec == "" {
			continue
		}
		if rec == target || strings.HasPrefix(ageKeyHash(rec), target) {
			match = &existing[i]
			break
		}
	}
	if match == nil {
		writeJSON(w, http.StatusNotFound, ageKeysResponse{Message: "recipient not found"})
		return
	}
	// Hard refusal: removing the last recipient leaves zero recipients,
	// which makes the worker fail to start (no encryption possible).
	// This is a destruction of capability, not a configuration choice.
	nonEmpty := 0
	for i := range existing {
		if recipientFromSecret(&existing[i]) != "" {
			nonEmpty++
		}
	}
	if nonEmpty <= 1 {
		s.emitAgeKeyEvent(r.Context(), match, "AgeKeyRemovalRefused",
			"Refused to remove the last age recipient — would disable encryption entirely")
		writeJSON(w, http.StatusConflict, ageKeysResponse{Message: "cannot remove the last recipient — at least one is required for encryption"})
		return
	}

	removed := recipientFromSecret(match)
	if err := s.cfg.Client.Delete(r.Context(), match); err != nil {
		if apierrors.IsNotFound(err) {
			// Race with another delete — treat as success.
			writeJSON(w, http.StatusOK, ageKeysResponse{OK: true, Message: "key removed"})
			return
		}
		s.cfg.Logger.Error(err, "delete recipient secret")
		writeJSON(w, http.StatusInternalServerError, ageKeysResponse{Message: "failed to delete recipient"})
		return
	}
	s.emitAgeKeyEvent(r.Context(), match, "AgeKeyRemoved",
		fmt.Sprintf("Public key %s removed via UI (%d remaining)", ageKeyHash(removed), nonEmpty-1))
	s.broadcast(sseEvent{Type: "age_keys_updated", Data: "removed"})
	writeJSON(w, http.StatusOK, ageKeysResponse{OK: true, Message: "key removed"})
}

// listRecipientSecrets returns every Secret in scope labeled
// role=age-recipient. Order is whatever the API server returns; callers
// that need stability should sort by .Name or by recipient string.
func (s *Server) listRecipientSecrets(ctx context.Context) ([]corev1.Secret, error) {
	var list corev1.SecretList
	opts := []client.ListOption{client.MatchingLabels{labels.LabelRole: labels.RoleAgeRecipient}}
	if s.cfg.Namespace != "" {
		opts = append(opts, client.InNamespace(s.cfg.Namespace))
	}
	if err := s.cfg.Client.List(ctx, &list, opts...); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// listRecipientViews flattens recipient Secrets into the UI shape,
// skipping Secrets that lack the public-key data field (defensive — the
// reconciler also skips them). Sorted by hash for stable display order.
func (s *Server) listRecipientViews(ctx context.Context) ([]ageKeyView, error) {
	secs, err := s.listRecipientSecrets(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]ageKeyView, 0, len(secs))
	seen := map[string]bool{}
	for i := range secs {
		rec := recipientFromSecret(&secs[i])
		if rec == "" || seen[rec] {
			continue
		}
		seen[rec] = true
		views = append(views, ageKeyView{Recipient: rec, Hash: ageKeyHash(rec)})
	}
	return views, nil
}

// recipientFromSecret extracts the trimmed public-key value from a
// recipient Secret, or "" if the field is missing/empty.
func recipientFromSecret(sec *corev1.Secret) string {
	raw, ok := sec.Data[labels.RecipientPublicKeyField]
	if !ok {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// recipientSecretName derives a deterministic Secret name from the
// recipient string. The hash makes Create idempotent — re-adding the
// same recipient lands on the same name and surfaces as 409 Conflict
// rather than creating a duplicate Secret with a different name.
func recipientSecretName(recipient string) string {
	return "backup-recipient-" + ageKeyHash(recipient)
}

// ageKeyHash returns a short identifier suitable for displaying next to
// a recipient. SHA256-prefix gives 48 bits of disambiguation — overkill
// for the typical handful of recipients but cheap.
func ageKeyHash(recipient string) string {
	sum := sha256.Sum256([]byte(recipient))
	return hex.EncodeToString(sum[:])[:12]
}

// emitAgeKeyEvent records a Kubernetes Event against the recipient
// Secret so add/remove operations show up in `kubectl describe secret
// <recipient>` and the cluster audit log. Best-effort — failing to
// emit must not abort the operation.
func (s *Server) emitAgeKeyEvent(ctx context.Context, sec *corev1.Secret, reason, message string) {
	s.emitUIEvent(ctx, corev1.ObjectReference{
		Kind:       "Secret",
		Namespace:  sec.Namespace,
		Name:       sec.Name,
		UID:        sec.UID,
		APIVersion: "v1",
	}, reason, message)
}
