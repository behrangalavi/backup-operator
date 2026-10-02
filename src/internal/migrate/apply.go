package migrate

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"backup-operator/api/v1alpha1"
)

// WriteYAML prints the plan as a multi-document manifest suitable for
// `kubectl apply -f -` or a GitOps commit. Credentials Secrets are included
// with their plaintext stringData — the caller decides where that goes.
func WriteYAML(w io.Writer, plan *Plan) error {
	objs := make([]any, 0, len(plan.Destinations)+len(plan.Sources)+len(plan.Recipients)+len(plan.Secrets))
	for _, o := range plan.Destinations {
		objs = append(objs, o)
	}
	for _, o := range plan.Sources {
		objs = append(objs, o)
	}
	for _, o := range plan.Recipients {
		objs = append(objs, o)
	}
	for _, o := range plan.Secrets {
		objs = append(objs, o)
	}
	for _, o := range objs {
		b, err := yaml.Marshal(o)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "---\n%s", b); err != nil {
			return err
		}
	}
	return nil
}

// Apply creates every object in the plan. Objects that already exist are
// left untouched (idempotent re-runs), credentials Secrets are owned by the
// CR they belong to so deleting the CR cascades, and legacy Secrets are
// deleted only when deleteLegacy is set. The returned notes describe each
// skipped or deleted object.
func Apply(ctx context.Context, c client.Client, plan *Plan, deleteLegacy bool) ([]string, error) {
	var notes []string
	owners := map[string]metav1.OwnerReference{} // credentials secret name → owner

	for _, bd := range plan.Destinations {
		created, err := createIfAbsent(ctx, c, bd)
		if err != nil {
			return notes, fmt.Errorf("create backupdestination %s: %w", bd.Name, err)
		}
		if !created {
			notes = append(notes, "backupdestination "+bd.Name+" already exists; left untouched")
		}
		owners[bd.Spec.CredentialsSecretRef.Name] = ownerRef(bd, v1alpha1.KindBackupDestination)
	}
	for _, bs := range plan.Sources {
		created, err := createIfAbsent(ctx, c, bs)
		if err != nil {
			return notes, fmt.Errorf("create backupsource %s: %w", bs.Name, err)
		}
		if !created {
			notes = append(notes, "backupsource "+bs.Name+" already exists; left untouched")
		}
		owners[bs.Spec.Database.CredentialsSecretRef.Name] = ownerRef(bs, v1alpha1.KindBackupSource)
	}
	for _, rec := range plan.Recipients {
		created, err := createIfAbsent(ctx, c, rec)
		if err != nil {
			return notes, fmt.Errorf("create backuprecipient %s: %w", rec.Name, err)
		}
		if !created {
			notes = append(notes, "backuprecipient "+rec.Name+" already exists; left untouched")
		}
	}
	for _, sec := range plan.Secrets {
		if ref, ok := owners[sec.Name]; ok && ref.UID != "" {
			sec.OwnerReferences = []metav1.OwnerReference{ref}
		}
		created, err := createIfAbsent(ctx, c, sec)
		if err != nil {
			return notes, fmt.Errorf("create secret %s: %w", sec.Name, err)
		}
		if !created {
			notes = append(notes, "secret "+sec.Name+" already exists; left untouched")
		}
	}
	if deleteLegacy {
		for _, name := range plan.Legacy {
			ns := namespaceOf(plan)
			err := c.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
			if err != nil && !apierrors.IsNotFound(err) {
				return notes, fmt.Errorf("delete legacy secret %s: %w", name, err)
			}
			notes = append(notes, "deleted legacy secret "+name)
		}
	}
	return notes, nil
}

// createIfAbsent creates obj and reports whether it was created. On
// AlreadyExists it re-reads the live object so the caller sees its UID.
func createIfAbsent(ctx context.Context, c client.Client, obj client.Object) (bool, error) {
	err := c.Create(ctx, obj)
	if err == nil {
		return true, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return false, err
	}
	if gerr := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); gerr != nil {
		return false, gerr
	}
	return false, nil
}

func ownerRef(obj client.Object, kind string) metav1.OwnerReference {
	t := true
	return metav1.OwnerReference{
		APIVersion:         v1alpha1.GroupVersion.String(),
		Kind:               kind,
		Name:               obj.GetName(),
		UID:                obj.GetUID(),
		BlockOwnerDeletion: &t,
	}
}

func namespaceOf(plan *Plan) string {
	for _, o := range plan.Sources {
		return o.Namespace
	}
	for _, o := range plan.Destinations {
		return o.Namespace
	}
	for _, o := range plan.Recipients {
		return o.Namespace
	}
	return ""
}
