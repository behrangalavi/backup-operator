package controllers

import (
	"context"
	"strings"
	"testing"

	"backup-operator/api/v1alpha1"
	"backup-operator/internal/agekeys"
	"backup-operator/internal/labels"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func recipient(name, namespace, key string) *v1alpha1.BackupRecipient {
	return &v1alpha1.BackupRecipient{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       v1alpha1.BackupRecipientSpec{PublicKey: key},
	}
}

// runReconcile triggers the rebuild path once. The reconciler rebuilds
// from a List on every Reconcile call regardless of req, so any
// well-formed request exercises the merge.
func runReconcile(t *testing.T, c client.Client, ns string) {
	t.Helper()
	r := &RecipientReconciler{Client: c, Logger: logr.Discard(), Namespace: ns, MergedSecretName: "merged"}
	res, err := r.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("Reconcile must requeue periodically — drift repair of the merged Secret depends on it")
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
	t.Fatalf("merged secret missing %s field", labels.MergedRecipientsField)
	return ""
}

func TestRecipientReconciler_TwoRecipients_SortedAndJoined(t *testing.T) {
	const ns = "backup"
	c := newFakeClient(t, recipient("alice", ns, "age1zzz"), recipient("bob", ns, "age1aaa"))
	runReconcile(t, c, ns)
	if got, want := mergedSecretContent(t, c, ns), "age1aaa\nage1zzz"; got != want {
		t.Errorf("merged content mismatch:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestRecipientReconciler_DeleteRebuilds(t *testing.T) {
	const ns = "backup"
	c := newFakeClient(t, recipient("alice", ns, "age1aaa"), recipient("bob", ns, "age1bbb"))
	runReconcile(t, c, ns)
	if got := mergedSecretContent(t, c, ns); !strings.Contains(got, "age1bbb") {
		t.Fatalf("first reconcile did not include both recipients: %q", got)
	}
	if err := c.Delete(context.Background(), recipient("bob", ns, "")); err != nil {
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

func TestRecipientReconciler_DedupesIdenticalKeys(t *testing.T) {
	const ns = "backup"
	c := newFakeClient(t, recipient("a", ns, "age1same"), recipient("b", ns, "age1same"))
	runReconcile(t, c, ns)
	if got := mergedSecretContent(t, c, ns); got != "age1same" {
		t.Errorf("identical keys must collapse to one line, got %q", got)
	}
}

func TestRecipientReconciler_NoRecipientsProducesEmptyMerged(t *testing.T) {
	const ns = "backup"
	c := newFakeClient(t)
	runReconcile(t, c, ns)
	if got := mergedSecretContent(t, c, ns); got != "" {
		t.Errorf("empty recipient set should produce empty merged content, got: %q", got)
	}
}

// TestBootstrap_SeedsRecipientsFromEnv: the chart's agePublicKeys arrive as
// one env string; Bootstrap turns each into a BackupRecipient and builds the
// merged Secret before the manager starts.
func TestBootstrap_SeedsRecipientsFromEnv(t *testing.T) {
	const ns = "backup"
	c := newFakeClient(t)
	r := &RecipientReconciler{Client: c, Logger: logr.Discard(), Namespace: ns, MergedSecretName: "merged"}
	r.Bootstrap(context.Background(), c, []string{"age1aaa,age1bbb"})

	var list v1alpha1.BackupRecipientList
	if err := c.List(context.Background(), &list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("expected 2 seeded recipients, got %d", len(list.Items))
	}
	for i := range list.Items {
		if list.Items[i].Name != agekeys.ObjectName(list.Items[i].Spec.PublicKey) {
			t.Errorf("seeded recipient %q must use the deterministic name %q", list.Items[i].Name, agekeys.ObjectName(list.Items[i].Spec.PublicKey))
		}
	}
	if got := mergedSecretContent(t, c, ns); got != "age1aaa\nage1bbb" {
		t.Errorf("merged after bootstrap = %q", got)
	}
}

// TestBootstrap_Idempotent: re-running seeds nothing twice and keeps the
// merged content stable. Keys removed from the env are NOT deleted.
func TestBootstrap_Idempotent(t *testing.T) {
	const ns = "backup"
	c := newFakeClient(t)
	r := &RecipientReconciler{Client: c, Logger: logr.Discard(), Namespace: ns, MergedSecretName: "merged"}
	r.Bootstrap(context.Background(), c, []string{"age1aaa"})
	first := mergedSecretContent(t, c, ns)
	r.Bootstrap(context.Background(), c, []string{"age1aaa"})
	r.Bootstrap(context.Background(), c, nil) // env emptied: existing object survives
	second := mergedSecretContent(t, c, ns)
	if first != second || first != "age1aaa" {
		t.Errorf("bootstrap must be idempotent and never delete: first=%q second=%q", first, second)
	}
	var list v1alpha1.BackupRecipientList
	if err := c.List(context.Background(), &list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Errorf("expected exactly 1 recipient after repeated bootstraps, got %d", len(list.Items))
	}
}

func TestSplitRecipients(t *testing.T) {
	got := splitRecipients("age1a\n# comment\n\nage1b, age1c ,")
	want := []string{"age1a", "age1b", "age1c"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("splitRecipients = %v, want %v", got, want)
	}
}
