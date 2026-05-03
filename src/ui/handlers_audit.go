package ui

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// auditEntry is the JSON shape returned to the UI for a single audit event.
type auditEntry struct {
	Timestamp string `json:"timestamp"` // RFC3339
	Type      string `json:"type"`      // Normal | Warning
	Reason    string `json:"reason"`
	Component string `json:"component"`
	Object    string `json:"object"` // <kind>/<name>
	Namespace string `json:"namespace"`
	Message   string `json:"message"`
	Category  string `json:"category"` // backup | retention | config | keys | other (for UI filtering)
}

type auditResponse struct {
	OK      bool         `json:"ok"`
	Entries []auditEntry `json:"entries"`
	Total   int          `json:"total"`
	Limit   int          `json:"limit"`
}

// auditComponents are the Source.Component values written by our own
// emitters. Filtering on these keeps third-party / kubelet noise out of
// the audit view (e.g. CronJob's SuccessfulCreate, Job-controller events).
var auditComponents = map[string]bool{
	"backup-worker":       true,
	"backup-operator-ui":  true,
	"cronjob-reconciler":  true,
}

// reasonCategory maps a Reason to a UI filter group. Anything unknown
// falls into "other" so a future emitter still surfaces in the audit.
func reasonCategory(reason string) string {
	switch reason {
	case "BackupStarted", "BackupCompleted", "BackupFailed":
		return "backup"
	case "RetentionDelete":
		return "retention"
	case "AgeKeyAdded", "AgeKeyRemoved", "AgeKeyRemovalRefused":
		return "keys"
	case "SettingsUpdated", "SourceCreated", "SourceUpdated", "SourceDeleted",
		"DestinationCreated", "DestinationUpdated", "DestinationDeleted":
		return "config"
	default:
		return "other"
	}
}

// handleAuditLog serves GET /api/audit-log. Reads Events directly via
// the uncached API reader — Events can pile up in busy clusters and
// caching them via the manager's cache would force a continuous watch.
//
// Query params:
//   - limit: cap on returned entries (default 200, max 1000)
//   - category: filter to one of backup|retention|keys|config|other
func (s *Server) handleAuditLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, auditResponse{})
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	categoryFilter := r.URL.Query().Get("category")

	var list corev1.EventList
	if err := s.cfg.APIReader.List(r.Context(), &list, client.InNamespace(s.cfg.Namespace)); err != nil {
		s.cfg.Logger.Error(err, "list events for audit")
		writeJSON(w, http.StatusInternalServerError, auditResponse{})
		return
	}

	entries := make([]auditEntry, 0, len(list.Items))
	for _, e := range list.Items {
		if !auditComponents[e.Source.Component] {
			continue
		}
		category := reasonCategory(e.Reason)
		if categoryFilter != "" && category != categoryFilter {
			continue
		}
		ts := eventTimestamp(e)
		object := e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name
		entries = append(entries, auditEntry{
			Timestamp: ts.UTC().Format(time.RFC3339),
			Type:      e.Type,
			Reason:    e.Reason,
			Component: e.Source.Component,
			Object:    object,
			Namespace: e.InvolvedObject.Namespace,
			Message:   e.Message,
			Category:  category,
		})
	}
	// Newest first — operators triaging an incident scroll from the
	// most recent event backwards.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Timestamp > entries[j].Timestamp
	})
	total := len(entries)
	if len(entries) > limit {
		entries = entries[:limit]
	}
	writeJSON(w, http.StatusOK, auditResponse{
		OK:      true,
		Entries: entries,
		Total:   total,
		Limit:   limit,
	})
}

// eventTimestamp picks the most informative timestamp available. K8s
// Events have several time fields with different population rules
// across event versions; this prefers the one most likely to reflect
// when the event actually occurred.
func eventTimestamp(e corev1.Event) time.Time {
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.FirstTimestamp.IsZero() {
		return e.FirstTimestamp.Time
	}
	return e.CreationTimestamp.Time
}
