package controllers

import (
	"context"
	"strings"
	"testing"

	"backup-operator/internal/labels"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return s
}

func recipientSecret(name, namespace, key string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{labels.LabelRole: labels.RoleAgeRecipient},
		},
		Data: map[string][]byte{
			labels.RecipientPublicKeyField: []byte(key),
		},
	}
}

// runReconcile triggers the rebuild path once. The reconciler rebuilds
// from a List on every Reconcile call regardless of req, so any
// well-formed request exercises the merge.
func runReconcile(t *testing.T, c client.Client, ns string) {
	t.Helper()
	r := &RecipientReconciler{
		Client:           c,
		Logger:           logr.Discard(),
		Namespace:        ns,
		MergedSecretName: "merged",
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func mergedSecretContent(t *testing.T, c client.Client, ns string) string {
	t.Helper()
	got := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "merged"}, got); err != nil {
		t.Fatalf("Get merged: %v", err)
	}
	if v, ok := got.Data[labels.MergedRecipientsField]; ok {
		return string(v)
	}
	if v, ok := got.StringData[labels.MergedRecipientsField]; ok {
		return v
	}
	t.Fatalf("merged secret missing %s field", labels.MergedRecipientsField)
	return ""
}

func TestRecipientReconciler_TwoRecipients_SortedAndJoined(t *testing.T) {
	const ns = "backup"
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(
			recipientSecret("alice", ns, "age1zzz"),
			recipientSecret("bob", ns, "age1aaa"),
		).
		Build()

	runReconcile(t, c, ns)

	got := mergedSecretContent(t, c, ns)
	want := "age1aaa\nage1zzz" // sort.Strings → asc
	if got != want {
		t.Errorf("merged content mismatch:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestRecipientReconciler_DeleteRebuilds(t *testing.T) {
	const ns = "backup"
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(
			recipientSecret("alice", ns, "age1aaa"),
			recipientSecret("bob", ns, "age1bbb"),
		).
		Build()

	runReconcile(t, c, ns)
	if got := mergedSecretContent(t, c, ns); !strings.Contains(got, "age1aaa") || !strings.Contains(got, "age1bbb") {
		t.Fatalf("first reconcile did not include both recipients: %q", got)
	}

	// Delete one recipient and reconcile again — merged must drop it.
	if err := c.Delete(context.Background(), recipientSecret("bob", ns, "")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	runReconcile(t, c, ns)

	got := mergedSecretContent(t, c, ns)
	if strings.Contains(got, "age1bbb") {
		t.Errorf("deleted recipient should be gone, but merged still has it: %q", got)
	}
	if !strings.Contains(got, "age1aaa") {
		t.Errorf("alice should still be present, got: %q", got)
	}
}

func TestRecipientReconciler_SkipsMissingPublicKeyField(t *testing.T) {
	const ns = "backup"
	bad := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "malformed",
			Namespace: ns,
			Labels:    map[string]string{labels.LabelRole: labels.RoleAgeRecipient},
		},
		// no Data → no public-key field
	}
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(
			bad,
			recipientSecret("good", ns, "age1ok"),
		).
		Build()

	runReconcile(t, c, ns)

	got := mergedSecretContent(t, c, ns)
	if got != "age1ok" {
		t.Errorf("malformed recipient should be skipped, expected only good key. got: %q", got)
	}
}

func TestRecipientReconciler_NoRecipientsProducesEmptyMerged(t *testing.T) {
	const ns = "backup"
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		Build()

	runReconcile(t, c, ns)

	got := mergedSecretContent(t, c, ns)
	if got != "" {
		t.Errorf("empty recipient set should produce empty merged content, got: %q", got)
	}
}

// legacyMergedSecret simulates the pre-RecipientReconciler chart layout:
// one Secret with newline-separated AGE_PUBLIC_KEYS and no role=age-recipient
// children.
func legacyMergedSecret(name, namespace string, keys []string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data: map[string][]byte{
			labels.MergedRecipientsField: []byte(strings.Join(keys, "\n")),
		},
	}
}

func TestBootstrap_MigratesLegacy(t *testing.T) {
	const ns = "backup"
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(
			legacyMergedSecret("merged", ns, []string{"age1aaa", "age1bbb"}),
		).
		Build()

	r := &RecipientReconciler{
		Client:           c,
		Logger:           logr.Discard(),
		Namespace:        ns,
		MergedSecretName: "merged",
	}
	r.Bootstrap(context.Background(), c)

	var recipients corev1.SecretList
	if err := c.List(context.Background(), &recipients,
		client.InNamespace(ns),
		client.MatchingLabels{labels.LabelRole: labels.RoleAgeRecipient}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recipients.Items) != 2 {
		t.Fatalf("expected 2 per-recipient secrets, got %d", len(recipients.Items))
	}
	got := mergedSecretContent(t, c, ns)
	if !strings.Contains(got, "age1aaa") || !strings.Contains(got, "age1bbb") {
		t.Errorf("merged content should hold both migrated keys, got %q", got)
	}
}

func TestBootstrap_SkipsWhenRecipientsExist(t *testing.T) {
	const ns = "backup"
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(
			legacyMergedSecret("merged", ns, []string{"age1zzz"}),
			recipientSecret("alice", ns, "age1aaa"),
		).
		Build()

	r := &RecipientReconciler{
		Client:           c,
		Logger:           logr.Discard(),
		Namespace:        ns,
		MergedSecretName: "merged",
	}
	r.Bootstrap(context.Background(), c)

	// Migration must not have run — no extra recipient created from the
	// legacy "age1zzz" key. Only `alice` should remain.
	var recipients corev1.SecretList
	if err := c.List(context.Background(), &recipients,
		client.InNamespace(ns),
		client.MatchingLabels{labels.LabelRole: labels.RoleAgeRecipient}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recipients.Items) != 1 {
		t.Errorf("expected migration to be skipped (1 existing recipient), got %d total",
			len(recipients.Items))
	}
	got := mergedSecretContent(t, c, ns)
	if strings.Contains(got, "age1zzz") {
		t.Errorf("legacy key should not have been migrated, but merged contains it: %q", got)
	}
	if !strings.Contains(got, "age1aaa") {
		t.Errorf("alice should be in merged content, got: %q", got)
	}
}

func TestBootstrap_NoLegacy_NoOp(t *testing.T) {
	const ns = "backup"
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		Build()

	r := &RecipientReconciler{
		Client:           c,
		Logger:           logr.Discard(),
		Namespace:        ns,
		MergedSecretName: "merged",
	}
	// Should not panic / error even when nothing exists. Bootstrap logs
	// internally; we're checking that the merged Secret comes up empty.
	r.Bootstrap(context.Background(), c)

	got := mergedSecretContent(t, c, ns)
	if got != "" {
		t.Errorf("fresh install should produce empty merged content, got: %q", got)
	}
}

func TestBootstrap_Idempotent(t *testing.T) {
	const ns = "backup"
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(
			legacyMergedSecret("merged", ns, []string{"age1aaa"}),
		).
		Build()

	r := &RecipientReconciler{
		Client:           c,
		Logger:           logr.Discard(),
		Namespace:        ns,
		MergedSecretName: "merged",
	}
	r.Bootstrap(context.Background(), c)
	first := mergedSecretContent(t, c, ns)
	r.Bootstrap(context.Background(), c)
	second := mergedSecretContent(t, c, ns)
	if first != second {
		t.Errorf("bootstrap should be idempotent: first=%q second=%q", first, second)
	}

	var recipients corev1.SecretList
	if err := c.List(context.Background(), &recipients,
		client.InNamespace(ns),
		client.MatchingLabels{labels.LabelRole: labels.RoleAgeRecipient}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recipients.Items) != 1 {
		t.Errorf("second bootstrap should not duplicate recipient secrets; got %d", len(recipients.Items))
	}
}

func TestRecipientReconciler_Idempotent(t *testing.T) {
	const ns = "backup"
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(
			recipientSecret("alice", ns, "age1aaa"),
		).
		Build()

	runReconcile(t, c, ns)
	first := mergedSecretContent(t, c, ns)
	runReconcile(t, c, ns)
	second := mergedSecretContent(t, c, ns)
	if first != second {
		t.Errorf("reconcile should be idempotent: first=%q second=%q", first, second)
	}
}
