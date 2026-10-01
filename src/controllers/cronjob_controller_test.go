package controllers

import (
	"context"
	"testing"

	"backup-operator/api/v1alpha1"
	"backup-operator/internal/secrets"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("v1alpha1 AddToScheme: %v", err)
	}
	return s
}

// newFakeClient wires the status subresource for every CRD so
// Status().Update behaves like the real API server.
func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(cronScheme(t)).
		WithStatusSubresource(&v1alpha1.BackupSource{}, &v1alpha1.BackupDestination{}).
		WithObjects(objs...).
		Build()
}

func testSource(name, ns string) *v1alpha1.BackupSource {
	return &v1alpha1.BackupSource{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("uid-" + name), Generation: 1},
		Spec: v1alpha1.BackupSourceSpec{
			Schedule: "0 2 * * *",
			Database: v1alpha1.DatabaseSpec{
				Type:                 v1alpha1.DatabasePostgres,
				Host:                 "db.example.com",
				Username:             "backup",
				CredentialsSecretRef: v1alpha1.CredentialsSecretRef{Name: name + "-credentials"},
			},
		},
	}
}

func reconcileOnce(t *testing.T, c client.Client, name, ns string) {
	t.Helper()
	r := &CronJobReconciler{Client: c, Logger: logr.Discard(), Worker: WorkerSpec{
		Image: "img", ServiceAccountName: "sa", AgeSecretName: "age", TempDir: "/tmp/x", RunTimeoutSeconds: 10, DefaultSchedule: "0 3 * * *",
	}}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func getSource(t *testing.T, c client.Client, name, ns string) *v1alpha1.BackupSource {
	t.Helper()
	var bs v1alpha1.BackupSource
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, &bs); err != nil {
		t.Fatalf("Get BackupSource: %v", err)
	}
	return &bs
}

// TestReconcile_CreatesOwnedCronJob is the happy path: a valid BackupSource
// yields a CronJob owned by it (so deletion cascades), pointing the worker
// at the source by name, and a Ready=True status with the materialised
// schedule recorded.
func TestReconcile_CreatesOwnedCronJob(t *testing.T) {
	src := testSource("prod-db", "backup")
	c := newFakeClient(t, src)
	reconcileOnce(t, c, "prod-db", "backup")

	var cj batchv1.CronJob
	if err := c.Get(context.Background(), types.NamespacedName{Name: "backup-prod-db", Namespace: "backup"}, &cj); err != nil {
		t.Fatalf("CronJob not created: %v", err)
	}
	if len(cj.OwnerReferences) != 1 || cj.OwnerReferences[0].Kind != v1alpha1.KindBackupSource || cj.OwnerReferences[0].UID != src.UID {
		t.Errorf("CronJob must be owned by the BackupSource, got %+v", cj.OwnerReferences)
	}
	args := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Args
	if len(args) < 2 || args[0] != "--source" || args[1] != "prod-db" {
		t.Errorf("worker must be pointed at the BackupSource by name, got args %v", args)
	}
	if cj.Spec.Suspend == nil || *cj.Spec.Suspend {
		t.Error("Suspend must be an explicit false")
	}

	got := getSource(t, c, "prod-db", "backup")
	ready := apimeta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition = %+v, want True", ready)
	}
	if got.Status.CronJobName != "backup-prod-db" {
		t.Errorf("status.cronJobName = %q", got.Status.CronJobName)
	}
	if got.Status.MaterializedSchedule == "" || got.Status.NextScheduleTime == nil {
		t.Errorf("status must carry the materialised schedule and next fire time, got %+v", got.Status)
	}
	if got.Status.ObservedGeneration != 1 {
		t.Errorf("observedGeneration = %d, want 1", got.Status.ObservedGeneration)
	}
}

// TestReconcile_SuspendedFlowsToCronJob: spec.suspended is the single source
// of truth for CronJob.Spec.Suspend, in both directions.
func TestReconcile_SuspendedFlowsToCronJob(t *testing.T) {
	src := testSource("paused", "backup")
	src.Spec.Suspended = true
	c := newFakeClient(t, src)
	reconcileOnce(t, c, "paused", "backup")

	var cj batchv1.CronJob
	if err := c.Get(context.Background(), types.NamespacedName{Name: "backup-paused", Namespace: "backup"}, &cj); err != nil {
		t.Fatal(err)
	}
	if cj.Spec.Suspend == nil || !*cj.Spec.Suspend {
		t.Error("CronJob must be suspended")
	}
	got := getSource(t, c, "paused", "backup")
	if got.Status.NextScheduleTime != nil {
		t.Error("a suspended source has no next schedule time")
	}
}

// TestReconcile_InvalidSpec_SetsReadyFalse: a source whose spec can't be
// flattened (traversal target name) produces no CronJob and a Ready=False
// condition carrying the reason, instead of a silent skip.
func TestReconcile_InvalidSpec_SetsReadyFalse(t *testing.T) {
	src := testSource("bad", "backup")
	src.Spec.TargetName = "../other-tenant"
	c := newFakeClient(t, src)
	reconcileOnce(t, c, "bad", "backup")

	var cj batchv1.CronJob
	if err := c.Get(context.Background(), types.NamespacedName{Name: "backup-bad", Namespace: "backup"}, &cj); err == nil {
		t.Error("no CronJob must be created for an invalid source")
	}
	got := getSource(t, c, "bad", "backup")
	ready := apimeta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "InvalidSpec" {
		t.Fatalf("Ready condition = %+v, want False/InvalidSpec", ready)
	}
}

// TestReconcile_DefaultScheduleApplied: a source without spec.schedule runs
// on the operator default, and status records the result.
func TestReconcile_DefaultScheduleApplied(t *testing.T) {
	src := testSource("nosched", "backup")
	src.Spec.Schedule = ""
	src.Spec.JitterMinutes = ptrInt32(0) // pin so the assertion is exact
	c := newFakeClient(t, src)
	reconcileOnce(t, c, "nosched", "backup")

	var cj batchv1.CronJob
	if err := c.Get(context.Background(), types.NamespacedName{Name: "backup-nosched", Namespace: "backup"}, &cj); err != nil {
		t.Fatal(err)
	}
	if cj.Spec.Schedule != "0 3 * * *" {
		t.Errorf("CronJob schedule = %q, want operator default", cj.Spec.Schedule)
	}
	if got := getSource(t, c, "nosched", "backup"); got.Status.MaterializedSchedule != "0 3 * * *" {
		t.Errorf("status.materializedSchedule = %q", got.Status.MaterializedSchedule)
	}
}

// TestReconcile_Idempotent: a second reconcile neither errors nor rewrites.
func TestReconcile_Idempotent(t *testing.T) {
	src := testSource("twice", "backup")
	c := newFakeClient(t, src)
	reconcileOnce(t, c, "twice", "backup")
	first := getSource(t, c, "twice", "backup")
	reconcileOnce(t, c, "twice", "backup")
	second := getSource(t, c, "twice", "backup")
	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("second reconcile rewrote status (rv %s → %s); status writes must be change-only", first.ResourceVersion, second.ResourceVersion)
	}
}

func TestCronJobNameFor_Short(t *testing.T) {
	got := cronJobNameFor("my-source")
	want := "backup-my-source"
	if got != want {
		t.Errorf("cronJobNameFor(%q) = %q, want %q", "my-source", got, want)
	}
}

func TestCronJobNameFor_ExactLimit(t *testing.T) {
	// "backup-" is 7 chars, so a 45-char name gives exactly 52.
	name := "abcdefghijklmnopqrstuvwxyz0123456789012345678"
	got := cronJobNameFor(name)
	if len(got) != 52 {
		t.Errorf("expected length 52, got %d: %q", len(got), got)
	}
	if got != "backup-"+name {
		t.Errorf("should not hash when exactly at limit: got %q", got)
	}
}

func TestCronJobNameFor_Long_HasHash(t *testing.T) {
	long := "very-long-source-name-that-exceeds-the-52-character-kubernetes-limit"
	got := cronJobNameFor(long)
	if len(got) > 52 {
		t.Errorf("name too long: %d > 52: %q", len(got), got)
	}
	if got[len(got)-9] != '-' {
		t.Errorf("expected hash separator near end, got %q", got)
	}
}

func TestCronJobNameFor_LongCollision(t *testing.T) {
	a := "very-long-prefix-shared-between-two-sources-aaaaaaaaaa"
	b := "very-long-prefix-shared-between-two-sources-bbbbbbbbbb"
	if cronJobNameFor(a) == cronJobNameFor(b) {
		t.Errorf("collision: both produced %q", cronJobNameFor(a))
	}
}

// TestBuildCronJob_AppliesSchedulingKnobs: the optional pod-shape knobs land
// in the worker pod template and the built-in env/volumes are preserved.
func TestBuildCronJob_AppliesSchedulingKnobs(t *testing.T) {
	src := testSource("knobs", "backup")
	flat, _ := secrets.FromBackupSource(src, nil, "0 2 * * *")
	r := &CronJobReconciler{Logger: logr.Discard(), Worker: WorkerSpec{
		Image: "img", ServiceAccountName: "sa", AgeSecretName: "age", TempDir: "/tmp/x", RunTimeoutSeconds: 10,
		NodeSelector:      map[string]string{"pool": "backup"},
		Tolerations:       []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "backup", Effect: corev1.TaintEffectNoSchedule}},
		Affinity:          &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}},
		PriorityClassName: "backup-low",
		PodAnnotations:    map[string]string{"sidecar.istio.io/inject": "false"},
		ExtraEnv:          []corev1.EnvVar{{Name: "PGSSLMODE", Value: "require"}},
		ExtraVolumes:      []corev1.Volume{{Name: "ca", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "db-ca"}}}},
		ExtraVolumeMounts: []corev1.VolumeMount{{Name: "ca", MountPath: "/etc/db-ca", ReadOnly: true}},
	}}
	cj := r.buildCronJob(src, flat)
	pod := cj.Spec.JobTemplate.Spec.Template
	if pod.Spec.NodeSelector["pool"] != "backup" || len(pod.Spec.Tolerations) != 1 || pod.Spec.Affinity == nil || pod.Spec.PriorityClassName != "backup-low" {
		t.Errorf("scheduling knobs not applied: %+v", pod.Spec)
	}
	if pod.Annotations["sidecar.istio.io/inject"] != "false" {
		t.Errorf("pod annotations not applied: %v", pod.Annotations)
	}
	c := pod.Spec.Containers[0]
	hasEnv := func(name string) bool {
		for _, e := range c.Env {
			if e.Name == name {
				return true
			}
		}
		return false
	}
	if !hasEnv("PGSSLMODE") || !hasEnv("AGE_PUBLIC_KEYS") {
		t.Errorf("extra env must be appended to, not replace, the built-in env: %v", c.Env)
	}
	foundMount, foundVol := false, false
	for _, m := range c.VolumeMounts {
		if m.Name == "ca" {
			foundMount = true
		}
	}
	for _, v := range pod.Spec.Volumes {
		if v.Name == "ca" {
			foundVol = true
		}
	}
	if !foundMount || !foundVol || len(pod.Spec.Volumes) < 2 {
		t.Errorf("extra volume/mount must be appended alongside the temp volume: vols=%v mounts=%v", pod.Spec.Volumes, c.VolumeMounts)
	}
}
