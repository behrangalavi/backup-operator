package controllers

import (
	"context"
	"testing"

	"backup-operator/api/v1alpha1"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testDestination(name, ns, secret string) *v1alpha1.BackupDestination {
	return &v1alpha1.BackupDestination{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.BackupDestinationSpec{
			Type:                 v1alpha1.StorageS3,
			CredentialsSecretRef: v1alpha1.LocalSecretRef{Name: secret},
			S3:                   &v1alpha1.S3Spec{Bucket: "b"},
		},
	}
}

// TestListBackupObjects_SkipsDestinationWithoutSecret: a destination whose
// credentials Secret is missing is dropped (and logged) rather than taking
// the whole listing down with it; the healthy one is still resolved with
// its credentials merged into Data.
func TestListBackupObjects_SkipsDestinationWithoutSecret(t *testing.T) {
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ok-creds", Namespace: "backup"},
		Data:       map[string][]byte{"access-key-id": []byte("k"), "secret-access-key": []byte("s")},
	}
	c := newFakeClient(t,
		testDestination("ok", "backup", "ok-creds"),
		testDestination("broken", "backup", "missing-creds"),
		testSource("src", "backup"),
		creds,
	)
	res, err := listBackupObjects(context.Background(), c, "backup", logr.Discard())
	if err != nil {
		t.Fatalf("listBackupObjects: %v", err)
	}
	if len(res.Sources) != 1 {
		t.Errorf("sources = %d, want 1", len(res.Sources))
	}
	if len(res.Dests) != 1 || res.Dests[0].Name != "ok" {
		t.Fatalf("expected only the resolvable destination, got %+v", res.Dests)
	}
	if string(res.Dests[0].Data["secret-access-key"]) != "s" || string(res.Dests[0].Data["bucket"]) != "b" {
		t.Errorf("destination Data must merge spec and credentials, got %v", res.Dests[0].Data)
	}
	if res.DestObjects["ok"] == nil {
		t.Error("DestObjects must index the raw CR by name")
	}
}
