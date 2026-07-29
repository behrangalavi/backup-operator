package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"backup-operator/internal/labels"
	"backup-operator/internal/scheduler"
	"backup-operator/internal/secrets"
	"backup-operator/metrics"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
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
	BackoffLimit       int32
	RetentionDaysDef   string
	MinKeepDef         string
	DefaultSchedule    string
	ImagePullSecrets   []corev1.LocalObjectReference
	Resources          corev1.ResourceRequirements
}

// CronJobReconciler keeps a managed K8s CronJob in sync with each source
// Secret. It does not run backups itself — workloads execute in CronJob-spawned
// Job pods running the worker binary.
//
// Lifecycle:
//   - Source Secret created/updated → ensure CronJob exists with correct spec
//   - Role label removed     → delete the managed CronJob (rare, but supported)
//   - Source Secret deleted  → CronJob is GC'd via OwnerReference, no work here
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
		For(&corev1.Secret{}, builder.WithPredicates(roleLabelTransitionPredicate())).
		Owns(&batchv1.CronJob{}).
		Complete(r)
}

func (r *CronJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Logger.WithValues("secret", req.NamespacedName)

	var sec corev1.Secret
	err := r.Client.Get(ctx, req.NamespacedName, &sec)
	if apierrors.IsNotFound(err) {
		// CronJob is GC'd via OwnerReference; nothing to do.
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	role := sec.Labels[labels.LabelRole]
	switch role {
	case labels.RoleSource:
		return ctrl.Result{}, r.ensureCronJob(ctx, &sec, log)
	case labels.RoleDestination:
		// Destinations are discovered by workers at run time; no managed
		// object. But a Secret relabelled source→destination previously WAS a
		// source and still owns the CronJob the source branch created — leaving
		// it would keep spawning worker Jobs against a Secret that is now a
		// destination. Clean it up (only if we actually own it).
		return ctrl.Result{}, r.deleteCronJobIfOwned(ctx, &sec, log)
	default:
		// Role label was removed — clean up any CronJob we previously owned.
		return ctrl.Result{}, r.deleteCronJobIfOwned(ctx, &sec, log)
	}
}

func (r *CronJobReconciler) ensureCronJob(ctx context.Context, sec *corev1.Secret, log logr.Logger) error {
	if unknown := secrets.WarnUnknownAnnotations(sec.Annotations); len(unknown) > 0 {
		log.Info("secret has unrecognised backup.mogenius.io annotations (possible typo)",
			"unknown", strings.Join(unknown, ", "))
	}

	src, err := secrets.ParseSource(sec, r.Worker.DefaultSchedule)
	if err != nil {
		log.Error(err, "skipping invalid source secret")
		metrics.IncSourceParseError(sec.Name)
		return nil
	}

	desired := r.buildCronJob(sec, src)
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
		// Log the materialised schedule (post-jitter), not the
		// annotation value — operators inspecting the log need to know
		// what actually runs, not what was requested.
		log.Info("cronjob reconciled", "operation", op, "target", src.TargetName,
			"schedule", desired.Spec.Schedule, "schedule_annotation", src.Schedule)
	}
	return nil
}

// deleteCronJobIfOwned deletes the managed CronJob for this Secret, but ONLY
// after confirming the operator actually owns it. The name alone
// (backup-<secretName>) is not proof of ownership: a user may run their own
// unrelated CronJob that happens to collide with that name, and deleting it
// would be destructive. We Get the object, verify it carries an
// OwnerReference back to this Secret's UID, and delete under a UID
// precondition so a same-named object created between Get and Delete is not
// removed by mistake.
func (r *CronJobReconciler) deleteCronJobIfOwned(ctx context.Context, sec *corev1.Secret, log logr.Logger) error {
	name := cronJobNameFor(sec.Name)
	var cj batchv1.CronJob
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: sec.Namespace, Name: name}, &cj)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get cronjob: %w", err)
	}

	if !ownedBySecret(&cj, sec) {
		log.Info("cronjob with matching name exists but is not owned by this secret; leaving it untouched",
			"cronjob", name)
		return nil
	}

	// UID precondition: only delete THIS object. If it was deleted and a
	// different one recreated with the same name in the meantime, the
	// precondition makes the delete a no-op/conflict rather than clobbering it.
	err = r.Client.Delete(ctx, &cj, client.Preconditions{UID: &cj.UID})
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete cronjob: %w", err)
	}
	log.Info("cronjob deleted (source role removed or changed)", "cronjob", name)
	return nil
}

// ownedBySecret reports whether the CronJob carries a controller
// OwnerReference back to the given Secret's UID — the same reference
// buildCronJob sets, and the one K8s GC uses for cascade delete. UID (not
// name) is the authoritative signal: an unrelated user CronJob sharing the
// name will not reference this Secret's UID.
func ownedBySecret(cj *batchv1.CronJob, sec *corev1.Secret) bool {
	for _, ref := range cj.OwnerReferences {
		if ref.Kind == "Secret" && ref.UID == sec.UID {
			return true
		}
	}
	return false
}

// buildCronJob produces the desired CronJob spec for a parsed source. The
// pod runs the worker binary against the source's Secret name; everything
// else (destinations, encryption keys) is discovered at run time.
func (r *CronJobReconciler) buildCronJob(sec *corev1.Secret, src *secrets.Source) *batchv1.CronJob {
	name := cronJobNameFor(sec.Name)
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
			"--source-secret", sec.Name,
			"--namespace", sec.Namespace,
		},
		ImagePullPolicy: r.Worker.ImagePullPolicy,
		Env:             r.workerEnv(sec.Namespace),
		SecurityContext: workerSecCtx,
		Resources:       r.Worker.Resources,
		VolumeMounts: r.workerVolumeMounts(),
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
			Namespace: sec.Namespace,
			Labels:    managedLabels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "v1",
					Kind:               "Secret",
					Name:               sec.Name,
					UID:                sec.UID,
					Controller:         ptr(true),
					BlockOwnerDeletion: ptr(true),
				},
			},
		},
		Spec: batchv1.CronJobSpec{
			Schedule:                   scheduler.ApplyJitter(src.Schedule, src.JitterMinutes, src.SecretName),
			ConcurrencyPolicy:          concurrency,
			// Pointer (not bare bool): we want the reconciler to deterministically
			// drive Suspend from the annotation, including back to false. Manual
			// `kubectl patch cronjob ... suspend=true` is overridden on next
			// reconcile — the Secret is the source of truth.
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

func (r *CronJobReconciler) workerEnv(namespace string) []corev1.EnvVar {
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

func cronJobNameFor(secretName string) string {
	const prefix = "backup-"
	const max = 52 // CronJob names are k8s names; leave headroom for Job suffix
	name := prefix + secretName
	if len(name) <= max {
		return name
	}
	// Append a hash suffix to prevent collisions when truncating long names.
	h := sha256.Sum256([]byte(secretName))
	suffix := hex.EncodeToString(h[:4]) // 8 hex chars
	return name[:max-len(suffix)-1] + "-" + suffix
}

// roleLabelTransitionPredicate ensures we reconcile when:
//   - a Secret with our role label is created/updated/deleted, OR
//   - a Secret had the role label and lost it (so we can clean up the CronJob)
func roleLabelTransitionPredicate() predicate.Predicate {
	hasRole := func(l map[string]string) bool {
		v := l[labels.LabelRole]
		return v == labels.RoleSource || v == labels.RoleDestination
	}
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return hasRole(e.Object.GetLabels()) },
		DeleteFunc: func(e event.DeleteEvent) bool { return hasRole(e.Object.GetLabels()) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return hasRole(e.ObjectOld.GetLabels()) || hasRole(e.ObjectNew.GetLabels())
		},
		GenericFunc: func(e event.GenericEvent) bool { return hasRole(e.Object.GetLabels()) },
	}
}

func ptr[T any](v T) *T       { return &v }
func ptrInt32(v int32) *int32 { return &v }
func ptrInt64(v int64) *int64 { return &v }
