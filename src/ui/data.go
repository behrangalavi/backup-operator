package ui

import (
	"context"
	"fmt"
	"sort"
	"time"

	"backup-operator/internal/labels"
	"backup-operator/internal/meta"
	"backup-operator/internal/secrets"
	storageFactory "backup-operator/storage/factory"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// dataSource is the read-side surface the handlers depend on. Kept narrow so
// it's trivial to swap with a fake in tests.
type dataSource interface {
	listTargets(ctx context.Context) ([]targetSummary, error)
	target(ctx context.Context, name string) (*targetDetail, error)
}

// targetSummary is what the index page needs per source.
type targetSummary struct {
	Name         string
	SecretName   string
	DBType       string
	Schedule     string
	Destinations []string
	CreatedAt    time.Time      // Secret CreationTimestamp; read off raw corev1 at access time
	Latest       *meta.MetaFile // nil if no runs yet
}

// targetDetail backs the per-source detail page.
type targetDetail struct {
	Source       *secrets.Source
	Destinations []*secrets.Destination
	Runs         []*meta.MetaFile
}

// k8sData implements dataSource using a controller-runtime client to enumerate
// labelled Secrets in the watched namespace and the storage abstraction to
// fetch meta.json files from destinations.
type k8sData struct {
	client    client.Client
	namespace string
	log       logr.Logger

	latestCache *cache[map[string]*meta.MetaFile] // per-destination → target→meta
	runsCache   *cache[[]*meta.MetaFile]          // per (target,destination)
}

func newK8sData(c client.Client, namespace string, log logr.Logger) *k8sData {
	return &k8sData{
		client:      c,
		namespace:   namespace,
		log:         log,
		latestCache: newCache[map[string]*meta.MetaFile](30 * time.Second),
		runsCache:   newCache[[]*meta.MetaFile](30 * time.Second),
	}
}

func (d *k8sData) listTargets(ctx context.Context) ([]targetSummary, error) {
	sources, createdAt, err := d.listSourceSecretsWithMeta(ctx)
	if err != nil {
		return nil, err
	}
	dests, err := d.listDestinationSecrets(ctx)
	if err != nil {
		return nil, err
	}

	// Pull "latest meta per target" once per destination, then merge by
	// target. The first destination that has a recorded run for a target
	// wins; that's good enough for an overview row.
	latestByTarget := map[string]*meta.MetaFile{}
	for _, dest := range dests {
		perDest, err := d.latestCache.getOrLoad(dest.Name, func() (map[string]*meta.MetaFile, error) {
			st, err := storageFactory.NewStorage(dest.StorageType, dest.Name, dest.Data, d.log.WithName("storage"))
			if err != nil {
				return nil, err
			}
			return meta.LatestPerTarget(ctx, st)
		})
		if err != nil {
			d.log.V(1).Info("destination unreadable for latest-summary", "destination", dest.Name, "err", err.Error())
			continue
		}
		for tgt, m := range perDest {
			existing, exists := latestByTarget[tgt]
			if !exists {
				latestByTarget[tgt] = m
			} else if existing.IsFailure() && !m.IsFailure() && m.Timestamp >= existing.Timestamp {
				latestByTarget[tgt] = m
			} else if m.Timestamp > existing.Timestamp {
				latestByTarget[tgt] = m
			}
		}
	}

	out := make([]targetSummary, 0, len(sources))
	for _, src := range sources {
		summary := targetSummary{
			Name:         src.TargetName,
			SecretName:   src.SecretName,
			DBType:       src.DBType,
			Schedule:     src.Schedule,
			Destinations: destinationsAllowedFor(src, dests),
			CreatedAt:    createdAt[src.SecretName],
			Latest:       latestByTarget[src.TargetName],
		}
		out = append(out, summary)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (d *k8sData) target(ctx context.Context, name string) (*targetDetail, error) {
	sources, err := d.listSourceSecrets(ctx)
	if err != nil {
		return nil, err
	}
	var src *secrets.Source
	for _, s := range sources {
		if s.TargetName == name {
			src = s
			break
		}
	}
	if src == nil {
		return nil, fmt.Errorf("target %q not found", name)
	}

	allDests, err := d.listDestinationSecrets(ctx)
	if err != nil {
		return nil, err
	}
	dests := secrets.FilterDestinations(src, allDests)
	if len(dests) == 0 {
		return &targetDetail{Source: src, Destinations: nil, Runs: nil}, nil
	}

	// Merge run history from ALL destinations. Each destination may have
	// runs that others don't (e.g. partial upload failures). Deduplicate
	// by timestamp, preferring the meta from the destination it was fetched
	// from so SourceDestination is set for download routing.
	byTimestamp := map[string]*meta.MetaFile{}
	for _, dest := range dests {
		key := name + "@" + dest.Name
		got, err := d.runsCache.getOrLoad(key, func() ([]*meta.MetaFile, error) {
			st, err := storageFactory.NewStorage(dest.StorageType, dest.Name, dest.Data, d.log.WithName("storage"))
			if err != nil {
				return nil, err
			}
			return meta.List(ctx, st, name)
		})
		if err != nil {
			d.log.V(1).Info("destination unreadable for run history", "destination", dest.Name, "err", err.Error())
			continue
		}
		for _, m := range got {
			existing, ok := byTimestamp[m.Timestamp]
			if !ok {
				byTimestamp[m.Timestamp] = m
			} else if existing.IsFailure() && !m.IsFailure() {
				byTimestamp[m.Timestamp] = m
			}
		}
	}
	runs := make([]*meta.MetaFile, 0, len(byTimestamp))
	for _, m := range byTimestamp {
		runs = append(runs, m)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Timestamp > runs[j].Timestamp })
	return &targetDetail{Source: src, Destinations: dests, Runs: runs}, nil
}

func (d *k8sData) listSourceSecrets(ctx context.Context) ([]*secrets.Source, error) {
	return d.listParsedSecrets(ctx, labels.RoleSource)
}

// listSourceSecretsWithMeta returns parsed sources alongside a secretName→creationTimestamp
// map so UI callers can sort by creation time without bloating secrets.Source itself.
func (d *k8sData) listSourceSecretsWithMeta(ctx context.Context) ([]*secrets.Source, map[string]time.Time, error) {
	var list corev1.SecretList
	if err := d.client.List(ctx, &list, client.InNamespace(d.namespace), client.MatchingLabels{
		labels.LabelRole: labels.RoleSource,
	}); err != nil {
		return nil, nil, err
	}
	out := make([]*secrets.Source, 0, len(list.Items))
	createdAt := make(map[string]time.Time, len(list.Items))
	for i := range list.Items {
		src, err := secrets.ParseSource(&list.Items[i], "")
		if err != nil {
			d.log.V(1).Info("skipping invalid source", "secret", list.Items[i].Name, "err", err.Error())
			continue
		}
		out = append(out, src)
		createdAt[list.Items[i].Name] = list.Items[i].CreationTimestamp.Time
	}
	return out, createdAt, nil
}

func (d *k8sData) listDestinationSecrets(ctx context.Context) ([]*secrets.Destination, error) {
	var list corev1.SecretList
	if err := d.client.List(ctx, &list, client.InNamespace(d.namespace), client.MatchingLabels{
		labels.LabelRole: labels.RoleDestination,
	}); err != nil {
		return nil, err
	}
	out := make([]*secrets.Destination, 0, len(list.Items))
	for i := range list.Items {
		dest, err := secrets.ParseDestination(&list.Items[i])
		if err != nil {
			d.log.V(1).Info("skipping invalid destination", "secret", list.Items[i].Name, "err", err.Error())
			continue
		}
		out = append(out, dest)
	}
	return out, nil
}

func (d *k8sData) listParsedSecrets(ctx context.Context, role string) ([]*secrets.Source, error) {
	var list corev1.SecretList
	if err := d.client.List(ctx, &list, client.InNamespace(d.namespace), client.MatchingLabels{
		labels.LabelRole: role,
	}); err != nil {
		return nil, err
	}
	out := make([]*secrets.Source, 0, len(list.Items))
	for i := range list.Items {
		// Default schedule is unimportant for UI display; we only consume
		// the parsed source's own metadata, never trigger runs from it.
		src, err := secrets.ParseSource(&list.Items[i], "")
		if err != nil {
			d.log.V(1).Info("skipping invalid source", "secret", list.Items[i].Name, "err", err.Error())
			continue
		}
		out = append(out, src)
	}
	return out, nil
}

// findRun resolves a (target, timestamp) pair to the MetaFile that the
// detail page already loaded. Returns nil if the timestamp doesn't match
// a known run.
func findRun(runs []*meta.MetaFile, timestamp string) *meta.MetaFile {
	for _, r := range runs {
		if r.Timestamp == timestamp {
			return r
		}
	}
	return nil
}

// destinationsAllowedFor returns the sorted names of destinations the source's
// allow-list permits, used purely for display. Delegates to secrets.FilterDestinations
// for the actual filtering logic.
func destinationsAllowedFor(src *secrets.Source, all []*secrets.Destination) []string {
	filtered := secrets.FilterDestinations(src, all)
	names := make([]string, len(filtered))
	for i, d := range filtered {
		names[i] = d.Name
	}
	sort.Strings(names)
	return names
}

