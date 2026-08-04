package controllers

import (
	"context"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"backup-operator/internal/labels"
	"backup-operator/internal/secrets"
)

// SecretListResult holds the result of listing source and destination Secrets.
type SecretListResult struct {
	Sources  []corev1.Secret
	Dests    []*secrets.Destination
	SrcRV    string // ResourceVersion of the source SecretList
	DestRV   string // ResourceVersion of the destination SecretList
}

// listBackupSecrets lists source and destination Secrets in the given
// namespace. If namespace is empty, all namespaces are searched.
// Invalid destination Secrets are skipped. Destinations sharing a logical
// name (backup.mogenius.io/name) are de-duplicated — the first wins, the rest
// are skipped with a warning — because the logical name keys the storage pool,
// the per-destination concurrency slots, and every per-destination metric
// label; two live destinations under one name silently corrupt all three.
func listBackupSecrets(ctx context.Context, c client.Client, namespace string, log logr.Logger) (*SecretListResult, error) {
	var srcList corev1.SecretList
	srcOpts := []client.ListOption{client.MatchingLabels{labels.LabelRole: labels.RoleSource}}
	if namespace != "" {
		srcOpts = append(srcOpts, client.InNamespace(namespace))
	}
	if err := c.List(ctx, &srcList, srcOpts...); err != nil {
		return nil, err
	}

	var destList corev1.SecretList
	destOpts := []client.ListOption{client.MatchingLabels{labels.LabelRole: labels.RoleDestination}}
	if namespace != "" {
		destOpts = append(destOpts, client.InNamespace(namespace))
	}
	if err := c.List(ctx, &destList, destOpts...); err != nil {
		return nil, err
	}

	dests := make([]*secrets.Destination, 0, len(destList.Items))
	seenName := make(map[string]string, len(destList.Items)) // logical name -> first Secret name
	for i := range destList.Items {
		d, err := secrets.ParseDestination(&destList.Items[i])
		if err != nil {
			continue
		}
		if first, dup := seenName[d.Name]; dup {
			log.Info("skipping destination with duplicate logical name; keeping the first",
				"name", d.Name, "kept", first, "skipped", d.SecretName)
			continue
		}
		seenName[d.Name] = d.SecretName
		dests = append(dests, d)
	}

	return &SecretListResult{
		Sources: srcList.Items,
		Dests:   dests,
		SrcRV:   srcList.ResourceVersion,
		DestRV:  destList.ResourceVersion,
	}, nil
}
