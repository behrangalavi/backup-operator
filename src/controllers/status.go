package controllers

import (
	"context"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"backup-operator/api/v1alpha1"
)

// updateSourceStatus re-reads the BackupSource, lets mutate rewrite its
// status, and writes it back only when something actually changed. The
// compare-before-write matters at scale: the refresher calls this for every
// source on every tick, and an unconditional Status().Update would turn a
// 30 s tick into N API writes (and N resourceVersion bumps that defeat the
// refresher's own list-RV skip). A missing object is not an error — it was
// deleted between list and write.
func updateSourceStatus(ctx context.Context, c client.Client, key types.NamespacedName, mutate func(*v1alpha1.BackupSourceStatus)) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var cur v1alpha1.BackupSource
		if err := c.Get(ctx, key, &cur); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		before := cur.Status.DeepCopy()
		mutate(&cur.Status)
		if equality.Semantic.DeepEqual(before, &cur.Status) {
			return nil
		}
		return c.Status().Update(ctx, &cur)
	})
}

// updateDestinationStatus is the BackupDestination twin of updateSourceStatus.
func updateDestinationStatus(ctx context.Context, c client.Client, key types.NamespacedName, mutate func(*v1alpha1.BackupDestinationStatus)) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var cur v1alpha1.BackupDestination
		if err := c.Get(ctx, key, &cur); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		before := cur.Status.DeepCopy()
		mutate(&cur.Status)
		if equality.Semantic.DeepEqual(before, &cur.Status) {
			return nil
		}
		return c.Status().Update(ctx, &cur)
	})
}
