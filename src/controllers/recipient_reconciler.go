package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"backup-operator/internal/labels"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// RecipientReconciler discovers Secrets labeled role=age-recipient and
// materialises them into a single operator-managed merged Secret that
// worker pods mount. Mirrors the source/destination discovery model: drop
// a labeled Secret in the namespace, the operator picks it up.
//
// Each recipient Secret carries one age public key under the
// `public-key` data field. The reconciler joins them newline-separated
// into the merged Secret's AGE_PUBLIC_KEYS data field — the format the
// worker has always consumed via `secretKeyRef`.
//
// On every reconcile we rebuild the full merged Secret from scratch
// rather than incrementally diffing — list+merge is cheap (handful of
// recipients per cluster), and rebuilding is the only way to handle
// deletes correctly without our own state. CreateOrUpdate makes it
// idempotent.
type RecipientReconciler struct {
	Client           client.Client
	Logger           logr.Logger
	Namespace        string // where to find recipient Secrets and write the merged Secret
	MergedSecretName string // name of the operator-managed merged Secret
}

func (r *RecipientReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("backup-recipient-controller").
		For(&corev1.Secret{}, builder.WithPredicates(r.watchPredicate())).
		Complete(r)
}

// NeedLeaderElection ensures only the leader writes the merged Secret —
// concurrent operator replicas reconciling the same labeled Secret would
// race on the merged-Secret update. The race is harmless (last write
// wins, same content), but we serialize for cleanliness.
func (r *RecipientReconciler) NeedLeaderElection() bool { return true }

func (r *RecipientReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Logger.WithValues("trigger", req.NamespacedName)
	if err := r.rebuildMerged(ctx, r.Client, log); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// rebuildMerged lists every recipient Secret in scope, extracts the
// public-key data field, sorts for deterministic output, and writes the
// merged Secret. Empty recipient list produces an empty merged Secret —
// worker pods will then fail to start with "no age recipients
// configured", which is the correct loud-failure behavior.
// The client is passed explicitly rather than always using r.Client: the
// Reconcile path uses the manager-cached client, while Bootstrap (which runs
// before the manager's cache has started) must use a direct API-server
// client. Threading it as a parameter avoids mutating the shared r.Client
// field, which was a latent data race if Bootstrap ever overlapped a reconcile.
func (r *RecipientReconciler) rebuildMerged(ctx context.Context, c client.Client, log logr.Logger) error {
	var list corev1.SecretList
	opts := []client.ListOption{
		client.MatchingLabels{labels.LabelRole: labels.RoleAgeRecipient},
	}
	if r.Namespace != "" {
		opts = append(opts, client.InNamespace(r.Namespace))
	}
	if err := c.List(ctx, &list, opts...); err != nil {
		return fmt.Errorf("list recipient secrets: %w", err)
	}

	keys := make([]string, 0, len(list.Items))
	for i := range list.Items {
		s := &list.Items[i]
		raw, ok := s.Data[labels.RecipientPublicKeyField]
		if !ok || len(raw) == 0 {
			log.V(1).Info("recipient missing public-key field; skipping",
				"secret", s.Name)
			continue
		}
		line := strings.TrimSpace(string(raw))
		if line == "" {
			continue
		}
		keys = append(keys, line)
	}
	sort.Strings(keys)

	merged := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.MergedSecretName,
			Namespace: r.Namespace,
		},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, c, merged, func() error {
		if merged.Labels == nil {
			merged.Labels = map[string]string{}
		}
		merged.Labels["app.kubernetes.io/managed-by"] = "backup-operator"
		merged.Labels["app.kubernetes.io/component"] = "age-recipients"
		if merged.Type == "" {
			merged.Type = corev1.SecretTypeOpaque
		}
		// Write directly to Data instead of StringData. Real K8s
		// normalises StringData → Data on apply, but for unit tests
		// against a fake client (and any other in-process client) the
		// merge isn't done — the previous Data value would survive
		// alongside StringData and consumers reading Data first see
		// stale content. Data-only is unambiguous on every client.
		merged.StringData = nil
		merged.Data = map[string][]byte{
			labels.MergedRecipientsField: []byte(strings.Join(keys, "\n")),
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("upsert merged recipient secret: %w", err)
	}
	if op != controllerutil.OperationResultNone {
		log.Info("merged age recipients reconciled",
			"operation", op,
			"count", len(keys),
			"secret", r.MergedSecretName)
	}
	return nil
}

// Bootstrap runs the one-time legacy-format migration plus an
// up-front rebuild of the merged Secret. Intended to be called by
// main.go BEFORE the manager starts, so the merged Secret exists with
// the right content the moment worker pods could try to mount it.
//
// `bootstrapClient` is a non-cached client (the manager's cache hasn't
// started yet at call time); it talks directly to the API server.
//
// Bootstrap is idempotent and safe to run on every operator replica:
//   - migrateLegacy is a no-op once per-recipient Secrets exist
//   - rebuildMerged is a CreateOrUpdate (last write wins, same content)
//
// Failure here does NOT crash the operator — log and proceed. The
// reconciler will catch up once the manager is running and recipient
// Secret events arrive. Crashing on bootstrap would mean a typo in
// agePublicKeys takes down the whole operator.
func (r *RecipientReconciler) Bootstrap(ctx context.Context, bootstrapClient client.Client) {
	log := r.Logger.WithName("bootstrap")
	if err := r.migrateLegacy(ctx, bootstrapClient, log); err != nil {
		log.Error(err, "legacy age-secret migration failed; continuing")
	}
	if err := r.rebuildMerged(ctx, bootstrapClient, log); err != nil {
		log.Error(err, "initial merged-secret rebuild failed; continuing")
	}
}

// migrateLegacy detects the pre-RecipientReconciler layout — a single
// merged Secret containing AGE_PUBLIC_KEYS without any per-recipient
// Secrets — and fans it out into per-recipient Secrets so the new
// reconcile loop has data to chew on. Once any role=age-recipient
// Secret exists, the migration is considered done and skipped.
func (r *RecipientReconciler) migrateLegacy(ctx context.Context, c client.Client, log logr.Logger) error {
	// If even one recipient Secret already exists we treat the namespace
	// as already migrated. Two replicas racing the same migration both
	// see "none yet", both Create — second hits AlreadyExists which we
	// swallow. Net result: idempotent.
	var existing corev1.SecretList
	opts := []client.ListOption{
		client.MatchingLabels{labels.LabelRole: labels.RoleAgeRecipient},
	}
	if r.Namespace != "" {
		opts = append(opts, client.InNamespace(r.Namespace))
	}
	if err := c.List(ctx, &existing, opts...); err != nil {
		return fmt.Errorf("list recipient secrets for migration check: %w", err)
	}
	if len(existing.Items) > 0 {
		log.V(1).Info("recipient secrets already present; skipping legacy migration",
			"count", len(existing.Items))
		return nil
	}

	legacy := &corev1.Secret{}
	err := c.Get(ctx, client.ObjectKey{
		Namespace: r.Namespace,
		Name:      r.MergedSecretName,
	}, legacy)
	if apierrors.IsNotFound(err) {
		// Fresh install or already-cleaned upgrade. Nothing to migrate.
		return nil
	}
	if err != nil {
		return fmt.Errorf("get legacy age secret %q: %w", r.MergedSecretName, err)
	}

	raw, ok := legacy.Data[labels.MergedRecipientsField]
	if !ok || len(raw) == 0 {
		return nil
	}
	keys := splitRecipients(string(raw))
	if len(keys) == 0 {
		return nil
	}

	created := 0
	for _, key := range keys {
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      legacyRecipientSecretName(key),
				Namespace: r.Namespace,
				Labels: map[string]string{
					labels.LabelRole:                 labels.RoleAgeRecipient,
					"app.kubernetes.io/managed-by":   "backup-operator",
					"app.kubernetes.io/component":    "age-recipient-migrated",
				},
				Annotations: map[string]string{
					labels.AnnotationName: "migrated-" + recipientHash(key),
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				labels.RecipientPublicKeyField: []byte(key),
			},
		}
		if err := c.Create(ctx, sec); err != nil {
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			return fmt.Errorf("create migrated recipient %q: %w", sec.Name, err)
		}
		created++
	}
	if created > 0 {
		log.Info("migrated legacy age recipients to per-Secret form",
			"created", created,
			"from_secret", r.MergedSecretName)
	}
	return nil
}

// splitRecipients normalises a newline-separated recipient blob into a
// trimmed list. Comments (#) and blank lines are skipped, matching what
// the worker's crypto.NewFromPublicKeys accepts.
func splitRecipients(raw string) []string {
	out := make([]string, 0)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// legacyRecipientSecretName produces a stable per-recipient name from
// the public key. Same hash as the UI handler's recipientSecretName so
// re-adding the same key via UI lands on the same Secret name (Create
// would surface 409 Conflict instead of duplicating).
func legacyRecipientSecretName(key string) string {
	return "backup-recipient-" + recipientHash(key)
}

func recipientHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

// watchPredicate fires on (a) Secrets carrying role=age-recipient — so a
// recipient added, edited, or losing its label triggers a rebuild — AND (b)
// the operator-managed merged Secret itself, so an out-of-band delete or edit
// (kubectl delete, GitOps drift) is repaired on the next event instead of
// silently leaving worker pods unable to mount AGE_PUBLIC_KEYS until the next
// recipient change or an operator restart. Rebuilding the merged Secret writes
// it once, which fires one more event that converges to no-op (CreateOrUpdate
// finds no diff), so there is no reconcile loop.
func (r *RecipientReconciler) watchPredicate() predicate.Predicate {
	relevant := func(o client.Object) bool {
		if o.GetLabels()[labels.LabelRole] == labels.RoleAgeRecipient {
			return true
		}
		return o.GetName() == r.MergedSecretName &&
			(r.Namespace == "" || o.GetNamespace() == r.Namespace)
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return relevant(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return relevant(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return relevant(e.ObjectOld) || relevant(e.ObjectNew) },
		GenericFunc: func(e event.GenericEvent) bool { return relevant(e.Object) },
	}
}
