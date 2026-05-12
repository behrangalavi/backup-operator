package controllers

import (
	"context"
	"sync"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// JobWatcher pushes SSE events to the UI whenever a backup-related
// Job transitions state (created, started, succeeded, failed, deleted).
// Without this, the UI only learned about job changes via the periodic
// 10s SSE refresh tick — a backup that finished in under 10s would
// not be visible until the next tick, and a running job's status
// would lag by up to that interval. With the watcher, the dashboard
// reflects job state within ~1s of the actual K8s event.
//
// The reconciler doesn't mutate anything — it's pure observability.
// We deliberately don't filter to "Jobs owned by our managed
// CronJobs" because:
//   - the operator runs in its own namespace, so every Job in scope
//     IS related to backups (no noise)
//   - a manual `kubectl create job --from=cronjob/backup-X` produces
//     a Job NOT owned by the CronJob; filtering would hide it from
//     the live UI, which is the opposite of what the user expects
//     when they trigger a backup by hand.
type JobWatcher struct {
	Client    client.Client
	Logger    logr.Logger
	Namespace string

	// Broadcast is called whenever a watched Job's status changes.
	// EventType is "job_state_change"; data is the Job name. Wired
	// from main.go to ui.Server.Broadcast — set to nil for tests or
	// when UI is disabled (the watcher becomes a cheap no-op).
	Broadcast func(eventType, data string)

	// lastSeen caches the last status conditions per Job so a reconcile
	// fired by a non-state-change event (label change, annotation edit,
	// etc.) doesn't generate a spurious SSE. Keyed by namespaced name.
	mu       sync.Mutex
	lastSeen map[string]string
}

func (r *JobWatcher) SetupWithManager(mgr ctrl.Manager) error {
	r.lastSeen = make(map[string]string)
	return ctrl.NewControllerManagedBy(mgr).
		Named("backup-job-watcher").
		For(&batchv1.Job{}, builder.WithPredicates(jobStatusPredicate())).
		Complete(r)
}

func (r *JobWatcher) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Namespace != "" && req.Namespace != r.Namespace {
		return ctrl.Result{}, nil
	}
	var job batchv1.Job
	err := r.Client.Get(ctx, req.NamespacedName, &job)
	if apierrors.IsNotFound(err) {
		// Job was deleted — clear our cache entry and notify the UI
		// so the row drops out of the live Jobs page.
		r.mu.Lock()
		delete(r.lastSeen, req.String())
		r.mu.Unlock()
		r.fire(req.NamespacedName, "deleted")
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	state := jobState(&job)
	r.mu.Lock()
	prev, hadPrev := r.lastSeen[req.String()]
	r.lastSeen[req.String()] = state
	r.mu.Unlock()
	if !hadPrev || prev != state {
		r.fire(req.NamespacedName, state)
	}
	return ctrl.Result{}, nil
}

func (r *JobWatcher) fire(nn types.NamespacedName, state string) {
	if r.Broadcast == nil {
		return
	}
	r.Logger.V(1).Info("job state change", "job", nn.String(), "state", state)
	r.Broadcast("job_state_change", nn.Name)
}

// jobState collapses a Job's status into a short string suitable for
// dedup. Mirrors the categories the UI's /api/jobs handler exposes,
// so an SSE "job_state_change" with state X aligns with what the
// frontend will re-fetch.
func jobState(j *batchv1.Job) string {
	if j == nil {
		return "unknown"
	}
	if j.Status.CompletionTime != nil {
		return "completed"
	}
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == "True" {
			return "failed"
		}
		if c.Type == batchv1.JobComplete && c.Status == "True" {
			return "completed"
		}
	}
	if j.Status.Active > 0 {
		return "running"
	}
	return "pending"
}

// jobStatusPredicate keeps the watcher quiet for changes that don't
// affect the live status: label edits, annotation tweaks, owner-ref
// updates from K8s GC. Reconciles are still cheap (one Get + a map
// lookup), but the SSE-suppression upstream means the frontend's
// 200ms render-debounce has less burst to swallow.
func jobStatusPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return true },
		DeleteFunc: func(e event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldJob, ok1 := e.ObjectOld.(*batchv1.Job)
			newJob, ok2 := e.ObjectNew.(*batchv1.Job)
			if !ok1 || !ok2 {
				return true
			}
			// Status struct comparison is shallow but adequate: any
			// real state transition (Active, Conditions, CompletionTime,
			// Succeeded, Failed counts) changes one of these fields.
			return !jobStatusEqual(oldJob, newJob)
		},
		GenericFunc: func(e event.GenericEvent) bool { return false },
	}
}

func jobStatusEqual(a, b *batchv1.Job) bool {
	if a.Status.Active != b.Status.Active ||
		a.Status.Succeeded != b.Status.Succeeded ||
		a.Status.Failed != b.Status.Failed {
		return false
	}
	if (a.Status.CompletionTime == nil) != (b.Status.CompletionTime == nil) {
		return false
	}
	if len(a.Status.Conditions) != len(b.Status.Conditions) {
		return false
	}
	for i := range a.Status.Conditions {
		if a.Status.Conditions[i].Type != b.Status.Conditions[i].Type ||
			a.Status.Conditions[i].Status != b.Status.Conditions[i].Status {
			return false
		}
	}
	return true
}

