package controllers

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func jobScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := batchv1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return s
}

// --- jobState tests ---

func TestJobState(t *testing.T) {
	now := metav1.Now()
	cases := []struct {
		name string
		job  *batchv1.Job
		want string
	}{
		{"nil", nil, "unknown"},
		{"completion time set", &batchv1.Job{Status: batchv1.JobStatus{CompletionTime: &now}}, "completed"},
		{"failed condition", &batchv1.Job{Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: "True"}},
		}}, "failed"},
		{"complete condition", &batchv1.Job{Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: "True"}},
		}}, "completed"},
		{"active", &batchv1.Job{Status: batchv1.JobStatus{Active: 1}}, "running"},
		{"pending", &batchv1.Job{Status: batchv1.JobStatus{}}, "pending"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := jobState(c.job); got != c.want {
				t.Errorf("jobState = %q, want %q", got, c.want)
			}
		})
	}
}

// --- jobStatusEqual tests ---

func TestJobStatusEqual(t *testing.T) {
	now := metav1.Now()
	base := func() *batchv1.Job {
		return &batchv1.Job{Status: batchv1.JobStatus{
			Active:    1,
			Succeeded: 0,
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: "False"},
			},
		}}
	}

	if !jobStatusEqual(base(), base()) {
		t.Error("identical statuses should compare equal")
	}

	diffActive := base()
	diffActive.Status.Active = 0
	if jobStatusEqual(base(), diffActive) {
		t.Error("differing Active count should not be equal")
	}

	diffComplete := base()
	diffComplete.Status.CompletionTime = &now
	if jobStatusEqual(base(), diffComplete) {
		t.Error("one with CompletionTime set should not be equal")
	}

	diffCondCount := base()
	diffCondCount.Status.Conditions = nil
	if jobStatusEqual(base(), diffCondCount) {
		t.Error("differing condition count should not be equal")
	}

	diffCondStatus := base()
	diffCondStatus.Status.Conditions[0].Status = "True"
	if jobStatusEqual(base(), diffCondStatus) {
		t.Error("differing condition status should not be equal")
	}
}

// --- Reconcile tests ---

func runningJob(name, ns string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status:     batchv1.JobStatus{Active: 1},
	}
}

func TestJobWatcher_Reconcile_FiresOnFirstSightThenDedups(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(jobScheme(t)).
		WithObjects(runningJob("backup-prod-x", "backup")).
		Build()

	var fired []string
	w := &JobWatcher{
		Client:    c,
		Logger:    logr.Discard(),
		Namespace: "backup",
		Broadcast: func(_, data string) { fired = append(fired, data) },
	}
	w.lastSeen = make(map[string]string)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "backup", Name: "backup-prod-x"}}

	if _, err := w.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if len(fired) != 1 || fired[0] != "backup-prod-x" {
		t.Fatalf("expected one broadcast for the new running job, got %v", fired)
	}
	// Same state again → deduped, no new broadcast.
	if _, err := w.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if len(fired) != 1 {
		t.Errorf("unchanged state must not re-broadcast, got %v", fired)
	}
}

func TestJobWatcher_Reconcile_DeletedJobFires(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(jobScheme(t)).Build() // no objects → NotFound

	var fired int
	w := &JobWatcher{
		Client:    c,
		Logger:    logr.Discard(),
		Namespace: "backup",
		Broadcast: func(_, _ string) { fired++ },
	}
	w.lastSeen = map[string]string{"backup/backup-prod-x": "running"}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "backup", Name: "backup-prod-x"}}
	if _, err := w.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if fired != 1 {
		t.Errorf("deleted job should fire exactly one broadcast, got %d", fired)
	}
	if _, ok := w.lastSeen["backup/backup-prod-x"]; ok {
		t.Error("deleted job should be evicted from lastSeen cache")
	}
}

func TestJobWatcher_Reconcile_IgnoresOtherNamespace(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(jobScheme(t)).Build()
	var fired int
	w := &JobWatcher{
		Client:    c,
		Logger:    logr.Discard(),
		Namespace: "backup",
		Broadcast: func(_, _ string) { fired++ },
	}
	w.lastSeen = make(map[string]string)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "other-ns", Name: "some-job"}}
	if _, err := w.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if fired != 0 {
		t.Errorf("jobs outside the watched namespace must be ignored, fired=%d", fired)
	}
}

func TestJobWatcher_fire_NilBroadcastIsNoop(t *testing.T) {
	w := &JobWatcher{Logger: logr.Discard()} // Broadcast nil
	// Should not panic.
	w.fire(types.NamespacedName{Namespace: "backup", Name: "x"}, "running")
}
