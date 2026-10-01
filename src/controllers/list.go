package controllers

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"backup-operator/api/v1alpha1"
	"backup-operator/internal/secrets"
)

// BackupListResult is what one listing of the namespace's backup objects
// yields: raw sources (callers parse per source so a bad one is skipped
// individually) and already-resolved destinations with credentials.
type BackupListResult struct {
	Sources []v1alpha1.BackupSource
	Dests   []*secrets.Destination
	// DestObjects keeps the raw CR per destination name so status writers
	// can address the object the Destination was flattened from.
	DestObjects map[string]*v1alpha1.BackupDestination
	SrcRV       string // ResourceVersion of the BackupSourceList
	DestRV      string // ResourceVersion of the BackupDestinationList
}

// listBackupObjects lists BackupSources and BackupDestinations in the
// namespace (all namespaces when empty) and resolves each destination's
// credentials Secret. A destination whose Secret is missing or whose spec
// does not flatten is logged and skipped — one broken destination must not
// blank out monitoring for the rest.
//
// Secrets are read through the same client; the manager disables caching
// for Secrets, so these are direct API reads and the operator never holds
// an informer over every Secret in the namespace.
func listBackupObjects(ctx context.Context, c client.Client, namespace string, log logr.Logger) (*BackupListResult, error) {
	var srcList v1alpha1.BackupSourceList
	var opts []client.ListOption
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := c.List(ctx, &srcList, opts...); err != nil {
		return nil, fmt.Errorf("list backupsources: %w", err)
	}
	var destList v1alpha1.BackupDestinationList
	if err := c.List(ctx, &destList, opts...); err != nil {
		return nil, fmt.Errorf("list backupdestinations: %w", err)
	}

	res := &BackupListResult{
		Sources:     srcList.Items,
		Dests:       make([]*secrets.Destination, 0, len(destList.Items)),
		DestObjects: make(map[string]*v1alpha1.BackupDestination, len(destList.Items)),
		SrcRV:       srcList.ResourceVersion,
		DestRV:      destList.ResourceVersion,
	}
	for i := range destList.Items {
		bd := &destList.Items[i]
		d, err := resolveDestination(ctx, c, bd)
		if err != nil {
			log.V(1).Info("skipping destination", "destination", bd.Name, "err", err.Error())
			continue
		}
		res.Dests = append(res.Dests, d)
		res.DestObjects[d.Name] = bd
	}
	return res, nil
}

// resolveDestination fetches the credentials Secret and flattens the CR.
func resolveDestination(ctx context.Context, c client.Client, bd *v1alpha1.BackupDestination) (*secrets.Destination, error) {
	var creds corev1.Secret
	key := client.ObjectKey{Namespace: bd.Namespace, Name: bd.Spec.CredentialsSecretRef.Name}
	if err := c.Get(ctx, key, &creds); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("credentials secret %q not found", key.Name)
		}
		return nil, fmt.Errorf("get credentials secret %q: %w", key.Name, err)
	}
	return secrets.FromBackupDestination(bd, &creds)
}
