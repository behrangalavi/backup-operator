package controllers

import (
	"context"
	"testing"

	"backup-operator/internal/labels"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func cronScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 AddToScheme: %v", err)
	}
	if err := batchv1.AddToScheme(s); err != nil {
		t.Fatalf("batchv1 AddToScheme: %v", err)
	}
	return s
}

// ownedCronJob builds a CronJob with the controller OwnerReference the
// reconciler sets — as if a prior source reconcile had created it.
func ownedCronJob(name, namespace string, owner *corev1.Secret) *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1", Kind: "Secret",
				Name: owner.Name, UID: owner.UID,
				Controller: ptr(true), BlockOwnerDeletion: ptr(true),
			}},
		},
	}
}

func cronExists(t *testing.T, c client.Client, name, namespace string) bool {
	t.Helper()
	var cj batchv1.CronJob
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, &cj)
	if err == nil {
		return true
	}
	if apierrors.IsNotFound(err) {
		return false
	}
	t.Fatalf("unexpected Get error: %v", err)
	return false
}

// TestReconcile_SourceToDestination_DeletesOwnedCronJob covers the role
// transition: a Secret that was a source (owns backup-<name>) is relabelled
// destination; the now-orphan CronJob must be removed.
func TestReconcile_SourceToDestination_DeletesOwnedCronJob(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "prod-db", Namespace: "backup", UID: "secret-uid-1",
			Labels: map[string]string{labels.LabelRole: labels.RoleDestination},
		},
	}
	cj := ownedCronJob("backup-prod-db", "backup", sec)
	c := fake.NewClientBuilder().WithScheme(cronScheme(t)).WithObjects(sec, cj).Build()
	r := &CronJobReconciler{Client: c, Logger: logr.Discard()}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "prod-db", Namespace: "backup"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if cronExists(t, c, "backup-prod-db", "backup") {
		t.Error("CronJob owned by a now-destination Secret should have been deleted")
	}
}

// TestReconcile_RoleRemoved_DeletesOwnedCronJob covers the label-removed path.
func TestReconcile_RoleRemoved_DeletesOwnedCronJob(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "prod-db", Namespace: "backup", UID: "secret-uid-1",
			// no role label
		},
	}
	cj := ownedCronJob("backup-prod-db", "backup", sec)
	c := fake.NewClientBuilder().WithScheme(cronScheme(t)).WithObjects(sec, cj).Build()
	r := &CronJobReconciler{Client: c, Logger: logr.Discard()}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "prod-db", Namespace: "backup"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if cronExists(t, c, "backup-prod-db", "backup") {
		t.Error("CronJob should have been deleted after role label removal")
	}
}

// TestDeleteCronJobIfOwned_LeavesUnownedCronJob is the core safety property:
// a same-named CronJob the operator did NOT create must never be deleted.
func TestDeleteCronJobIfOwned_LeavesUnownedCronJob(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "redis", Namespace: "backup", UID: "secret-uid-redis",
		},
	}
	// A user's own unrelated CronJob that happens to collide on the name and
	// has no OwnerReference back to the Secret.
	foreign := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "backup-redis", Namespace: "backup"},
	}
	c := fake.NewClientBuilder().WithScheme(cronScheme(t)).WithObjects(sec, foreign).Build()
	r := &CronJobReconciler{Client: c, Logger: logr.Discard()}

	if err := r.deleteCronJobIfOwned(context.Background(), sec, logr.Discard()); err != nil {
		t.Fatalf("deleteCronJobIfOwned: %v", err)
	}
	if !cronExists(t, c, "backup-redis", "backup") {
		t.Error("a CronJob not owned by the operator must NOT be deleted")
	}
}

// TestDeleteCronJobIfOwned_WrongOwnerUID guards against a same-name CronJob
// owned by a DIFFERENT Secret (e.g. after a hash-suffix collision edge).
func TestDeleteCronJobIfOwned_WrongOwnerUID(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "redis", Namespace: "backup", UID: "secret-uid-A"},
	}
	other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "redis", Namespace: "backup", UID: "secret-uid-B"}}
	cj := ownedCronJob("backup-redis", "backup", other) // owned by a different UID
	c := fake.NewClientBuilder().WithScheme(cronScheme(t)).WithObjects(sec, cj).Build()
	r := &CronJobReconciler{Client: c, Logger: logr.Discard()}

	if err := r.deleteCronJobIfOwned(context.Background(), sec, logr.Discard()); err != nil {
		t.Fatalf("deleteCronJobIfOwned: %v", err)
	}
	if !cronExists(t, c, "backup-redis", "backup") {
		t.Error("CronJob owned by a different Secret UID must NOT be deleted")
	}
}

func TestCronJobNameFor_Short(t *testing.T) {
	got := cronJobNameFor("my-secret")
	want := "backup-my-secret"
	if got != want {
		t.Errorf("cronJobNameFor(%q) = %q, want %q", "my-secret", got, want)
	}
}

func TestCronJobNameFor_ExactLimit(t *testing.T) {
	// "backup-" is 7 chars, so a 45-char secret name gives exactly 52.
	secret := "abcdefghijklmnopqrstuvwxyz0123456789012345678"
	got := cronJobNameFor(secret)
	if len(got) != 52 {
		t.Errorf("expected length 52, got %d: %q", len(got), got)
	}
	want := "backup-" + secret
	if got != want {
		t.Errorf("should not hash when exactly at limit: got %q, want %q", got, want)
	}
}

func TestCronJobNameFor_Long_HasHash(t *testing.T) {
	long := "very-long-secret-name-that-exceeds-the-52-character-kubernetes-limit"
	got := cronJobNameFor(long)
	if len(got) > 52 {
		t.Errorf("name too long: %d > 52: %q", len(got), got)
	}
	// Must end with a hash suffix
	if got[len(got)-9] != '-' {
		t.Errorf("expected hash separator near end, got %q", got)
	}
}

func TestCronJobNameFor_LongCollision(t *testing.T) {
	// Two secrets that share the same 52-char prefix must produce different names.
	a := "very-long-prefix-shared-between-two-secrets-aaaaaaaaa"
	b := "very-long-prefix-shared-between-two-secrets-bbbbbbbbb"
	gotA := cronJobNameFor(a)
	gotB := cronJobNameFor(b)
	if gotA == gotB {
		t.Errorf("collision: both produced %q", gotA)
	}
}
