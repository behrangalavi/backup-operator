package controllers

import (
	"testing"

	"backup-operator/internal/labels"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// TestWatchPredicate_IncludesMergedSecret regresses the drift-repair gap: the
// reconciler must react to an out-of-band mutation of the merged Secret, not
// only to recipient Secrets, so a deleted/edited merged Secret is rebuilt.
func TestWatchPredicate_IncludesMergedSecret(t *testing.T) {
	r := &RecipientReconciler{Namespace: "backup", MergedSecretName: "backup-operator-age"}
	pred := r.watchPredicate()

	sec := func(name, ns, role string) *corev1.Secret {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		if role != "" {
			s.Labels = map[string]string{labels.LabelRole: role}
		}
		return s
	}

	cases := []struct {
		name string
		obj  *corev1.Secret
		want bool
	}{
		{"recipient secret", sec("rec-1", "backup", labels.RoleAgeRecipient), true},
		{"merged secret (no label)", sec("backup-operator-age", "backup", ""), true},
		{"merged secret wrong ns", sec("backup-operator-age", "other", ""), false},
		{"unrelated secret", sec("prod-db", "backup", labels.RoleSource), false},
	}
	for _, c := range cases {
		if got := pred.Create(event.CreateEvent{Object: c.obj}); got != c.want {
			t.Errorf("%s: Create = %v, want %v", c.name, got, c.want)
		}
		if got := pred.Delete(event.DeleteEvent{Object: c.obj}); got != c.want {
			t.Errorf("%s: Delete = %v, want %v", c.name, got, c.want)
		}
	}
}
