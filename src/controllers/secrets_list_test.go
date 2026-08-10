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

// TestListBackupSecrets_DedupesDuplicateDestinationNames confirms two
// destination Secrets sharing a logical name collapse to one (first wins), so
// the pool key / metric labels / concurrency slots stay unambiguous.
func TestListBackupSecrets_DedupesDuplicateDestinationNames(t *testing.T) {
	dest := func(secretName, logical string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: "backup",
				Labels:    map[string]string{labels.LabelRole: labels.RoleDestination, labels.LabelStorageType: "s3"},
				Annotations: map[string]string{labels.AnnotationName: logical},
			},
			Data: map[string][]byte{
				"bucket":            []byte("b"),
				"access-key-id":     []byte("k"),
				"secret-access-key": []byte("s"),
			},
		}
	}
	c := fake.NewClientBuilder().WithScheme(cronScheme(t)).
		WithObjects(dest("nas-a", "nas"), dest("nas-b", "nas"), dest("other", "offsite")).
		Build()

	res, err := listBackupSecrets(context.Background(), c, "backup", logr.Discard())
	if err != nil {
		t.Fatalf("listBackupSecrets: %v", err)
	}
	names := map[string]int{}
	for _, d := range res.Dests {
		names[d.Name]++
	}
	if names["nas"] != 1 {
		t.Errorf("duplicate logical name 'nas' must collapse to 1, got %d", names["nas"])
	}
	if names["offsite"] != 1 {
		t.Errorf("distinct 'offsite' must remain, got %d", names["offsite"])
	}
	if len(res.Dests) != 2 {
		t.Errorf("expected 2 destinations after dedup, got %d", len(res.Dests))
	}
}
