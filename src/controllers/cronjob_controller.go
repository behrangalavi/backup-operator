package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"time"

	"backup-operator/api/v1alpha1"
	"backup-operator/internal/scheduler"
	"backup-operator/internal/secrets"
	"backup-operator/metrics"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// WorkerSpec carries everything needed to template a CronJob's worker pod.
// Comes from the operator's env (which Helm wires) — it stays stable for the
// process lifetime, so we capture it once at controller construction.
type WorkerSpec struct {
	Image              string // container image (operator+worker share)
	ImagePullPolicy    corev1.PullPolicy
	ServiceAccountName string
	AgeSecretName      string // Secret holding AGE_PUBLIC_KEYS
	TempDir            string
	TempDirSize        string // e.g. "10Gi"
	RunTimeoutSeconds  int64
	// BackoffLimit caps K8s-native Job retries on failure. 0 = no retries
	// (one attempt, then fail). Default in main.go is 2; tunable via the
	// WORKER_BACKOFF_LIMIT env var so transient DB/network blips don't
	// cost a full cron interval of missing backups.
	BackoffLimit     int32
	RetentionDaysDef string
	MinKeepDef       string
	DefaultSchedule  string
	ImagePullSecrets []corev1.LocalObjectReference
	Resources        corev1.ResourceRequirements
}

// CronJobReconciler keeps a managed K8s CronJob in sync with each
// BackupSource. It does not run backups itself — workloads execute in
// CronJob-spawned Job pods running the worker binary.
//
// Lifecycle:
//   - BackupSource created/updated → ensure CronJob exists with correct spec
//   - BackupSource deleted         → CronJob is GC'd via OwnerReference
//
// Reconciles must remain idempotent: rerunning produces the same CronJob.
type CronJobReconciler struct {
	Client client.Client
	Scheme *runtime.Scheme
	Logger logr.Logger
	Worker WorkerSpec
}

func (r *CronJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("backup-cronjob-controller").
		// GenerationChangedPredicate on the source: the metrics refresher
		// writes run-level status on every landed run, and status writes
		// don't bump metadata.generation (status is a subresource), so this
		// reconciler only wakes for real spec edits.
		For(&v1alpha1.BackupSource{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// Same predicate on owned CronJobs: the CronJob controller bumps
		// status.lastScheduleTime on EVERY fire; without it each fire cost
		// one reconcile that re-computed an unchanged spec.
		Owns(&batchv1.CronJob{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

func (r *CronJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Logger.WithValues("backupsource", req.NamespacedName)

	var bs v1alpha1.BackupSource
	err := r.Client.Get(ctx, req.NamespacedName, &bs)
	if apierrors.IsNotFound(err) {
		// CronJob is GC'd via OwnerReference; nothing to do.
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.ensureCronJob(ctx, &bs, log)
}

func (r *CronJobReconciler) ensureCronJob(ctx context.Context, bs *v1alpha1.BackupSource, log logr.Logger) error {
	// Spec-only flatten: the CronJob template needs schedule/jitter/suspend
	// and the target name, never the password, so the credentials Secret is
	// not read here. The worker resolves it at run time.
	src, err := secrets.FromBackupSource(bs, nil, r.Worker.DefaultSchedule)
	if err != nil {
		log.Error(err, "skipping invalid backupsource")
		metrics.IncSourceParseError(bs.Name)
		return updateSourceStatus(ctx, r.Client, client.ObjectKeyFromObject(bs), func(st *v1alpha1.BackupSourceStatus) {
			st.ObservedGeneration = bs.Generation
			apimeta.SetStatusCondition(&st.Conditions, metav1.Condition{
				Type:               v1alpha1.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             "InvalidSpec",
				Message:            err.Error(),
				ObservedGeneration: bs.Generation,
			})
		})
	}

	desired := r.buildCronJob(bs, src)
	current := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, current, func() error {
		// Preserve immutable / status fields; copy spec + labels from desired.
		current.Labels = desired.Labels
		current.Annotations = desired.Annotations
		current.OwnerReferences = desired.OwnerReferences
		current.Spec = desired.Spec
		return nil
	})
	if err != nil {
		return fmt.Errorf("CreateOrUpdate cronjob: %w", err)
	}
	if op != controllerutil.OperationResultNone {
		// Log the materialised schedule (post-jitter), not the spec value —
		// operators inspecting the log need to know what actually runs.
		log.Info("cronjob reconciled", "operation", op, "target", src.TargetName,
			"schedule", desired.Spec.Schedule, "schedule_spec", src.Schedule)
	}

	return updateSourceStatus(ctx, r.Client, client.ObjectKeyFromObject(bs), func(st *v1alpha1.BackupSourceStatus) {
		st.ObservedGeneration = bs.Generation
		st.CronJobName = desired.Name
		st.MaterializedSchedule = desired.Spec.Schedule
		st.NextScheduleTime = nextScheduleTime(desired.Spec.Schedule, src.Suspended, time.Now())
		apimeta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             "CronJobReconciled",
			Message:            "managed CronJob " + desired.Name + " is in sync",
			ObservedGeneration: bs.Generation,
		})
	})
}

// nextScheduleTime is the status-shaped wrapper around scheduler.NextRun.
// Suspended sources get nil: there is no next fire while paused.
func nextScheduleTime(schedule string, suspended bool, now time.Time) *metav1.Time {
	if suspended {
		return nil
	}
	next, ok := scheduler.NextRun(schedule, now.UTC())
	if !ok {
		return nil
	}
	t := metav1.NewTime(next)
	return &t
}

// buildCronJob produces the desired CronJob spec for a parsed source. The
// pod runs the worker binary against the BackupSource's name; everything
// else (credentials, destinations, encryption keys) is resolved at run time.
func (r *CronJobReconciler) buildCronJob(bs *v1alpha1.BackupSource, src *secrets.Source) *batchv1.CronJob {
	name := cronJobNameFor(bs.Name)
	concurrency := batchv1.ForbidConcurrent

	managedLabels := map[string]string{
		"app.kubernetes.io/managed-by": "backup-operator",
		"backup.mogenius.io/target":    src.TargetName,
	}

	workerSecCtx := &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr(false),
		ReadOnlyRootFilesystem:   ptr(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}

	containers := []corev1.Container{{
		Name:    "worker",
		Image:   r.Worker.Image,
		Command: []string{"/app/backup-worker"},
		Args: []string{
			"--source", bs.Name,
			"--namespace", bs.Namespace,
		},
		ImagePullPolicy: r.Worker.ImagePullPolicy,
		Env:             r.workerEnv(),
		SecurityContext: workerSecCtx,
		Resources:       r.Worker.Resources,
		VolumeMounts:    r.workerVolumeMounts(),
	}}

	tempVolume := corev1.Volume{
		Name: "temp",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}
	if r.Worker.TempDirSize != "" {
		if q, err := resource.ParseQuantity(r.Worker.TempDirSize); err == nil {
			tempVolume.EmptyDir.SizeLimit = &q
		}
		// On parse error, leave SizeLimit unset rather than failing — the
		// volume still mounts, just without the size cap.
	}

	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: bs.Namespace,
			Labels:    managedLabels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         v1alpha1.GroupVersion.String(),
					Kind:               v1alpha1.KindBackupSource,
					Name:               bs.Name,
					UID:                bs.UID,
					Controller:         ptr(true),
					BlockOwnerDeletion: ptr(true),
				},
			},
		},
		Spec: batchv1.CronJobSpec{
			Schedule:          scheduler.ApplyJitter(src.Schedule, src.JitterMinutes, src.ObjectName),
			ConcurrencyPolicy: concurrency,
			// Pointer (not bare bool): we want the reconciler to deterministically
			// drive Suspend from spec.suspended, including back to false. Manual
			// `kubectl patch cronjob ... suspend=true` is overridden on next
			// reconcile — the BackupSource is the source of truth.
			Suspend:                    ptr(src.Suspended),
			SuccessfulJobsHistoryLimit: ptrInt32(3),
			FailedJobsHistoryLimit:     ptrInt32(3),
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					BackoffLimit:          ptrInt32(r.Worker.BackoffLimit),
					ActiveDeadlineSeconds: &r.Worker.RunTimeoutSeconds,
					// Auto-clean both scheduled and manually-triggered Jobs
					// 24h after they finish. Failure history lives in the
					// failure-meta sidecars in storage, so we don't need
					// K8s to keep stale Job objects around for audit.
					TTLSecondsAfterFinished: ptrInt32(86400),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: managedLabels},
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyNever,
							ServiceAccountName: r.Worker.ServiceAccountName,
							ImagePullSecrets:   r.Worker.ImagePullSecrets,
							SecurityContext: &corev1.PodSecurityContext{
								RunAsNonRoot:   ptr(true),
								RunAsUser:      ptrInt64(1000),
								RunAsGroup:     ptrInt64(1000),
								FSGroup:        ptrInt64(1000),
								SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
							},
							Containers: containers,
							Volumes:    r.workerVolumes(tempVolume),
						},
					},
				},
			},
		},
	}
}

func (r *CronJobReconciler) workerVolumeMounts() []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: "temp", MountPath: r.Worker.TempDir},
	}
	if r.Worker.TempDir != "/tmp" && !strings.HasPrefix(r.Worker.TempDir, "/tmp/") {
		mounts = append(mounts, corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"})
	}
	return mounts
}

func (r *CronJobReconciler) workerVolumes(tempVolume corev1.Volume) []corev1.Volume {
	vols := []corev1.Volume{tempVolume}
	if r.Worker.TempDir != "/tmp" && !strings.HasPrefix(r.Worker.TempDir, "/tmp/") {
		vols = append(vols, corev1.Volume{
			Name: "tmp",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
	}
	return vols
}

func (r *CronJobReconciler) workerEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{
			Name:      "POD_NAMESPACE",
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}},
		},
		// Downward API: worker Pod's own name + uid lets the
		// restore-verifier spawn ephemeral DB pods OwnerReference'd to
		// the worker, so they cascade-delete on worker exit. Without
		// these, Phase 2 verifiers can't clean up after themselves.
		{
			Name:      "POD_NAME",
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}},
		},
		{
			Name:      "POD_UID",
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}},
		},
		{
			Name: "AGE_PUBLIC_KEYS",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: r.Worker.AgeSecretName},
					Key:                  "AGE_PUBLIC_KEYS",
				},
			},
		},
		{Name: "RUN_TIMEOUT_SECONDS", Value: fmt.Sprintf("%d", r.Worker.RunTimeoutSeconds)},
		{Name: "TEMP_DIR", Value: r.Worker.TempDir},
		{Name: "DEFAULT_RETENTION_DAYS", Value: r.Worker.RetentionDaysDef},
		{Name: "DEFAULT_MIN_KEEP", Value: r.Worker.MinKeepDef},
		{Name: "DEFAULT_SCHEDULE", Value: r.Worker.DefaultSchedule},
	}
}

// cronJobNameFor derives the managed CronJob name from the BackupSource
// name. Stable across reconciles by construction.
func cronJobNameFor(sourceName string) string {
	const prefix = "backup-"
	const max = 52 // CronJob names are k8s names; leave headroom for Job suffix
	name := prefix + sourceName
	if len(name) <= max {
		return name
	}
	// Append a hash suffix to prevent collisions when truncating long names.
	h := sha256.Sum256([]byte(sourceName))
	suffix := hex.EncodeToString(h[:4]) // 8 hex chars
	return name[:max-len(suffix)-1] + "-" + suffix
}

func ptr[T any](v T) *T       { return &v }
func ptrInt32(v int32) *int32 { return &v }
func ptrInt64(v int64) *int64 { return &v }
