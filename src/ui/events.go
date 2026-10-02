package ui

import (
	"context"
	"os"
	"time"

	"backup-operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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
