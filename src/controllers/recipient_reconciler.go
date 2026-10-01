package controllers

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"backup-operator/api/v1alpha1"
	"backup-operator/internal/agekeys"
	"backup-operator/internal/labels"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// mergedResyncInterval bounds how long an out-of-band edit or delete of the
// merged Secret can go unnoticed. The reconciler deliberately does not watch
// Secrets — that would put an informer over every Secret in the namespace
// into the operator — so drift repair rides on a periodic requeue instead.
// Worker pods that race into the window fail to mount AGE_PUBLIC_KEYS and
// the Job retries, which is loud and self-healing.
const mergedResyncInterval = 10 * time.Minute

// RecipientReconciler merges every BackupRecipient in the namespace into a
// single operator-managed Secret that worker pods mount. Each recipient
// carries one age public key; the merged Secret's AGE_PUBLIC_KEYS data field
// is the newline-joined list — the format the worker has always consumed via
// `secretKeyRef`, so the worker contract did not change with the CRD move.
//
// On every reconcile the full merged Secret is rebuilt from a List rather
// than diffed incrementally — list+merge is cheap (a handful of recipients
// per cluster) and rebuilding is the only way to handle deletes correctly
// without our own state. CreateOrUpdate makes it idempotent.
type RecipientReconciler struct {
	Client           client.Client
	Logger           logr.Logger
	Namespace        string // where to find recipients and write the merged Secret
	MergedSecretName string // name of the operator-managed merged Secret
}

func (r *RecipientReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("backup-recipient-controller").
		For(&v1alpha1.BackupRecipient{}).
		Complete(r)
}

// NeedLeaderElection ensures only the leader writes the merged Secret —
// concurrent operator replicas reconciling the same recipient would race
// on the merged-Secret update. The race is harmless (last write wins, same
// content), but we serialize for cleanliness.
func (r *RecipientReconciler) NeedLeaderElection() bool { return true }

func (r *RecipientReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Logger.WithValues("trigger", req.NamespacedName)
	if err := r.rebuildMerged(ctx, r.Client, log); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: mergedResyncInterval}, nil
}

// rebuildMerged lists every BackupRecipient in scope, sorts the keys for
// deterministic output, and writes the merged Secret. An empty recipient
// list produces an empty merged Secret — worker pods then fail to start
// with "no age recipients configured", which is the correct loud failure.
//
// The client is a parameter because Bootstrap runs before the manager's
// cache has started and must use a direct API-server client, while
// Reconcile uses the manager client.
func (r *RecipientReconciler) rebuildMerged(ctx context.Context, c client.Client, log logr.Logger) error {
	var list v1alpha1.BackupRecipientList
	var opts []client.ListOption
	if r.Namespace != "" {
		opts = append(opts, client.InNamespace(r.Namespace))
	}
	if err := c.List(ctx, &list, opts...); err != nil {
		return fmt.Errorf("list backuprecipients: %w", err)
	}

	keys := make([]string, 0, len(list.Items))
	seen := make(map[string]bool, len(list.Items))
	for i := range list.Items {
		line := strings.TrimSpace(list.Items[i].Spec.PublicKey)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
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
		// Write directly to Data instead of StringData. Real K8s normalises
		// StringData → Data on apply, but the fake client used in tests does
		// not; Data-only is unambiguous on every client.
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

// Bootstrap seeds BackupRecipient objects from the chart-provided key list
// and rebuilds the merged Secret, BEFORE the manager starts, so the merged
// Secret exists with the right content the moment worker pods could mount
// it. The chart hands the keys over as an env var instead of rendering
// BackupRecipient manifests itself: Helm applies templated CRDs and CRs in
// one batch without waiting for the CRD to be established, so a CR in the
// chart fails the very first install with "no matches for kind".
//
// Idempotent and safe on every replica: Create ignores AlreadyExists, and
// rebuildMerged is a CreateOrUpdate. Keys removed from the chart values are
// NOT deleted here — the objects outlive the value that seeded them; remove
// them via kubectl or the UI.
//
// Failure does not crash the operator — log and proceed. The reconciler
// catches up once the manager runs. Crashing on bootstrap would mean a typo
// in agePublicKeys takes down the whole operator.
func (r *RecipientReconciler) Bootstrap(ctx context.Context, bootstrapClient client.Client, keys []string) {
	log := r.Logger.WithName("bootstrap")
	if err := r.seedRecipients(ctx, bootstrapClient, keys, log); err != nil {
		log.Error(err, "seeding bootstrap recipients failed; continuing")
	}
	if err := r.rebuildMerged(ctx, bootstrapClient, log); err != nil {
		log.Error(err, "initial merged-secret rebuild failed; continuing")
	}
}

func (r *RecipientReconciler) seedRecipients(ctx context.Context, c client.Client, keys []string, log logr.Logger) error {
	created := 0
	for _, key := range splitRecipients(strings.Join(keys, "\n")) {
		rec := &v1alpha1.BackupRecipient{
			ObjectMeta: metav1.ObjectMeta{
				Name:      agekeys.ObjectName(key),
				Namespace: r.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "backup-operator",
					"app.kubernetes.io/component":  "age-recipient-bootstrap",
				},
			},
			Spec: v1alpha1.BackupRecipientSpec{PublicKey: key},
		}
		if err := c.Create(ctx, rec); err != nil {
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			return fmt.Errorf("create bootstrap recipient %q: %w", rec.Name, err)
		}
		created++
	}
	if created > 0 {
		log.Info("seeded bootstrap age recipients", "created", created)
	}
	return nil
}

// splitRecipients normalises a newline- or comma-separated recipient blob
// into a trimmed list. Comments (#) and blank entries are skipped, matching
// what the worker's crypto.NewFromPublicKeys accepts.
func splitRecipients(raw string) []string {
	out := make([]string, 0)
	for _, line := range strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == ',' }) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}
