package controllers

import (
	"context"
	"fmt"

	"backup-operator/internal/labels"
	"backup-operator/internal/secrets"

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
		// Destinations are discovered by workers at run time; no managed object.
		return ctrl.Result{}, nil
	default:
		// Role label was removed — clean up any CronJob we previously owned.
		return ctrl.Result{}, r.deleteCronJobIfOwned(ctx, &sec, log)
	}
}

func (r *CronJobReconciler) ensureCronJob(ctx context.Context, sec *corev1.Secret, log logr.Logger) error {
	src, err := secrets.ParseSource(sec, r.Worker.DefaultSchedule)
	if err != nil {
		log.Error(err, "skipping invalid source secret")
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
		log.Info("cronjob reconciled", "operation", op, "target", src.TargetName, "schedule", src.Schedule)
	}
	return nil
}

func (r *CronJobReconciler) deleteCronJobIfOwned(ctx context.Context, sec *corev1.Secret, log logr.Logger) error {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: cronJobNameFor(sec.Name), Namespace: sec.Namespace},
	}
	err := r.Client.Delete(ctx, cj)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete cronjob: %w", err)
	}
	log.Info("cronjob deleted (role label removed)", "cronjob", cj.Name)
	return nil
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
		VolumeMounts: []corev1.VolumeMount{
			{Name: "temp", MountPath: "/tmp"},
		},
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
			Schedule:                   src.Schedule,
			ConcurrencyPolicy:          concurrency,
			SuccessfulJobsHistoryLimit: ptrInt32(3),
			FailedJobsHistoryLimit:     ptrInt32(3),
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					BackoffLimit:          ptrInt32(0),
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
							Volumes:    []corev1.Volume{tempVolume},
						},
					},
				},
			},
		},
	}
}

func (r *CronJobReconciler) workerEnv(namespace string) []corev1.EnvVar {
	return []corev1.EnvVar{
		{
			Name:      "POD_NAMESPACE",
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}},
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
	if len(name) > max {
		return name[:max]
	}
	return name
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
