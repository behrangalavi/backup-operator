package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
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
	OK          bool         `json:"ok"`
	Message     string       `json:"message,omitempty"`
	Keys        []ageKeyView `json:"keys,omitempty"`
	CanMutate   bool         `json:"canMutate"`
	SecretName  string       `json:"secretName,omitempty"`
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
	if s.cfg.AgeSecretName == "" {
		writeJSON(w, http.StatusNotFound, ageKeysResponse{Message: "age secret not configured"})
		return
	}
	recipients, _, err := s.readAgeRecipients(r.Context())
	if err != nil {
		s.cfg.Logger.Error(err, "list age keys")
		writeJSON(w, http.StatusInternalServerError, ageKeysResponse{Message: "failed to read age secret"})
		return
	}
	views := make([]ageKeyView, 0, len(recipients))
	for _, rec := range recipients {
		views = append(views, ageKeyView{Recipient: rec, Hash: ageKeyHash(rec)})
	}
	writeJSON(w, http.StatusOK, ageKeysResponse{
		OK:         true,
		Keys:       views,
		CanMutate:  !s.cfg.ReadOnly && s.cfg.AllowKeyMutation,
		SecretName: s.cfg.AgeSecretName,
	})
}

func (s *Server) handleAddAgeKey(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AgeSecretName == "" {
		writeJSON(w, http.StatusNotFound, ageKeysResponse{Message: "age secret not configured"})
		return
	}
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
	// worker can't either, and we'd silently break encryption. The
	// crypto package only loads X25519 recipients today (see
	// crypto/age.go), so we mirror that constraint here.
	if _, err := age.ParseX25519Recipient(candidate); err != nil {
		writeJSON(w, http.StatusBadRequest, ageKeysResponse{Message: "invalid age recipient: " + err.Error()})
		return
	}
	recipients, sec, err := s.readAgeRecipients(r.Context())
	if err != nil {
		s.cfg.Logger.Error(err, "read age secret for add")
		writeJSON(w, http.StatusInternalServerError, ageKeysResponse{Message: "failed to read age secret"})
		return
	}
	for _, existing := range recipients {
		if existing == candidate {
			writeJSON(w, http.StatusConflict, ageKeysResponse{Message: "recipient already present"})
			return
		}
	}
	recipients = append(recipients, candidate)
	if err := s.writeAgeRecipients(r.Context(), sec, recipients); err != nil {
		s.cfg.Logger.Error(err, "update age secret")
		writeJSON(w, http.StatusInternalServerError, ageKeysResponse{Message: "failed to save age secret"})
		return
	}
	s.emitAgeKeyEvent(r.Context(), sec, "AgeKeyAdded", fmt.Sprintf("Public key %s added via UI (%d total)", ageKeyHash(candidate), len(recipients)))
	s.broadcast(sseEvent{Type: "age_keys_updated", Data: "added"})
	writeJSON(w, http.StatusCreated, ageKeysResponse{OK: true, Message: "key added"})
}

func (s *Server) handleDeleteAgeKey(w http.ResponseWriter, r *http.Request, target string) {
	if s.cfg.AgeSecretName == "" {
		writeJSON(w, http.StatusNotFound, ageKeysResponse{Message: "age secret not configured"})
		return
	}
	recipients, sec, err := s.readAgeRecipients(r.Context())
	if err != nil {
		s.cfg.Logger.Error(err, "read age secret for delete")
		writeJSON(w, http.StatusInternalServerError, ageKeysResponse{Message: "failed to read age secret"})
		return
	}
	// Match by recipient string OR by hash prefix — the UI sends the
	// full recipient, but allowing hash makes scripted use safer
	// against pasting issues with long base32 strings.
	idx := -1
	for i, rec := range recipients {
		if rec == target || strings.HasPrefix(ageKeyHash(rec), target) {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeJSON(w, http.StatusNotFound, ageKeysResponse{Message: "recipient not found"})
		return
	}
	// Hard refusal: removing the last recipient leaves zero recipients,
	// which makes the worker fail to start (no encryption possible).
	// This is a destruction of capability, not a configuration choice.
	if len(recipients) <= 1 {
		s.emitAgeKeyEvent(r.Context(), sec, "AgeKeyRemovalRefused", "Refused to remove the last age recipient — would disable encryption entirely")
		writeJSON(w, http.StatusConflict, ageKeysResponse{Message: "cannot remove the last recipient — at least one is required for encryption"})
		return
	}
	removed := recipients[idx]
	recipients = append(recipients[:idx], recipients[idx+1:]...)
	if err := s.writeAgeRecipients(r.Context(), sec, recipients); err != nil {
		s.cfg.Logger.Error(err, "update age secret")
		writeJSON(w, http.StatusInternalServerError, ageKeysResponse{Message: "failed to save age secret"})
		return
	}
	s.emitAgeKeyEvent(r.Context(), sec, "AgeKeyRemoved", fmt.Sprintf("Public key %s removed via UI (%d remaining)", ageKeyHash(removed), len(recipients)))
	s.broadcast(sseEvent{Type: "age_keys_updated", Data: "removed"})
	writeJSON(w, http.StatusOK, ageKeysResponse{OK: true, Message: "key removed"})
}

// readAgeRecipients fetches the age Secret and returns its current
// newline-separated recipient list plus the Secret object itself for
// downstream Update/Event-emission.
func (s *Server) readAgeRecipients(ctx context.Context) ([]string, *corev1.Secret, error) {
	sec := &corev1.Secret{}
	if err := s.cfg.Client.Get(ctx, client.ObjectKey{
		Namespace: s.cfg.Namespace,
		Name:      s.cfg.AgeSecretName,
	}, sec); err != nil {
		return nil, nil, err
	}
	raw := string(sec.Data["AGE_PUBLIC_KEYS"])
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, sec, nil
}

// writeAgeRecipients persists a recipient list back to the Secret. The
// stored value is newline-joined (worker reads it with the same split
// rules). Trailing newline omitted to keep the payload minimal.
func (s *Server) writeAgeRecipients(ctx context.Context, sec *corev1.Secret, recipients []string) error {
	if sec.Data == nil {
		sec.Data = make(map[string][]byte)
	}
	sec.Data["AGE_PUBLIC_KEYS"] = []byte(strings.Join(recipients, "\n"))
	return s.cfg.Client.Update(ctx, sec)
}

// ageKeyHash returns a short identifier suitable for displaying next to
// a recipient. SHA256-prefix gives 48 bits of disambiguation — overkill
// for the typical handful of recipients but cheap.
func ageKeyHash(recipient string) string {
	sum := sha256.Sum256([]byte(recipient))
	return hex.EncodeToString(sum[:])[:12]
}

// emitAgeKeyEvent records a Kubernetes Event against the age Secret so
// add/remove operations show up in `kubectl describe secret <age>` and
// the cluster audit log. Best-effort — failing to emit must not abort
// the Secret update.
func (s *Server) emitAgeKeyEvent(ctx context.Context, sec *corev1.Secret, reason, message string) {
	now := metav1.Now()
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: sec.Name + ".",
			Namespace:    sec.Namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:       "Secret",
			Namespace:  sec.Namespace,
			Name:       sec.Name,
			UID:        sec.UID,
			APIVersion: "v1",
		},
		Reason:         reason,
		Message:        message,
		Type:           corev1.EventTypeNormal,
		Source:         corev1.EventSource{Component: "backup-operator-ui"},
		EventTime:      metav1.NewMicroTime(time.Now()),
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}
	if err := s.cfg.Client.Create(ctx, event); err != nil {
		s.cfg.Logger.Error(err, "emit age key event", "reason", reason)
	}
}
