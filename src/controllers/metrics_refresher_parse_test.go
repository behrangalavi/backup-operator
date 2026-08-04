package controllers

import (
	"context"
	"testing"

	"backup-operator/internal/labels"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestRefresh_ParseErrorKeepsTargetSticky regresses the "monitoring goes dark"
// bug: when a still-present source Secret fails to parse (e.g. an invalid name
// annotation typed during an edit), the refresher must keep its target's series
// rather than sweeping them — otherwise BackupOverdue can never fire on the now
// absent last_success series and existing alerts silently resolve.
func TestRefresh_ParseErrorKeepsTargetSticky(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prod-db",
			Namespace: "backup",
			Labels: map[string]string{
				labels.LabelRole:   labels.RoleSource,
				labels.LabelDBType: "postgres",
			},
			// Invalid target name — ParseSource rejects it, so this tick
			// cannot compute the target from the Secret.
			Annotations: map[string]string{labels.AnnotationName: "../evil"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(cronScheme(t)).WithObjects(sec).Build()
	r := &MetricsRefresher{
		Client:         c,
		Namespace:      "backup",
		Logger:         logr.Discard(),
		trackedTargets: map[string]bool{"prod": true},
		secretToTarget: map[string]string{"backup/prod-db": "prod"},
	}

	r.refresh(context.Background())

	if !r.trackedTargets["prod"] {
		t.Error("a source that fails to parse must keep its target's metrics sticky, not delete them")
	}
}

// TestRefresh_RemovedSourceDropsTarget confirms the complementary case: a source
// that is genuinely gone (no backing Secret) still has its target swept and its
// secretToTarget entry pruned.
func TestRefresh_RemovedSourceDropsTarget(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(cronScheme(t)).Build()
	r := &MetricsRefresher{
		Client:         c,
		Namespace:      "backup",
		Logger:         logr.Discard(),
		trackedTargets: map[string]bool{"gone": true},
		secretToTarget: map[string]string{"backup/gone-db": "gone"},
	}

	r.refresh(context.Background())

	if r.trackedTargets["gone"] {
		t.Error("a removed source's target must be swept from tracking")
	}
	if _, ok := r.secretToTarget["backup/gone-db"]; ok {
		t.Error("secretToTarget must be pruned for a removed source")
	}
}
