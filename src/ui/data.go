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
	DBType       string
	Schedule     string
	Destinations []string
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
	sources, err := d.listSourceSecrets(ctx)
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
			if _, exists := latestByTarget[tgt]; !exists {
				latestByTarget[tgt] = m
			}
		}
	}

	out := make([]targetSummary, 0, len(sources))
	for _, src := range sources {
		summary := targetSummary{
			Name:         src.TargetName,
			DBType:       src.DBType,
			Schedule:     src.Schedule,
			Destinations: destinationsAllowedFor(src, dests),
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
	dests := filterDestinations(src, allDests)
	if len(dests) == 0 {
		return &targetDetail{Source: src, Destinations: nil, Runs: nil}, nil
	}

	// Read history from the first destination that responds. The other
	// destinations should hold the same artefacts (fan-out preserves
	// timestamps), so a single read is canonical for display.
	var runs []*meta.MetaFile
	for _, dest := range dests {
		key := name + "@" + dest.Name
		got, err := d.runsCache.getOrLoad(key, func() ([]*meta.MetaFile, error) {
			st, err := storageFactory.NewStorage(dest.StorageType, dest.Name, dest.Data, d.log.WithName("storage"))
			if err != nil {
				return nil, err
			}
			return meta.List(ctx, st, name)
		})
		if err == nil && len(got) > 0 {
			runs = got
			break
		}
	}
	return &targetDetail{Source: src, Destinations: dests, Runs: runs}, nil
}

func (d *k8sData) listSourceSecrets(ctx context.Context) ([]*secrets.Source, error) {
	return d.listParsedSecrets(ctx, labels.RoleSource)
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

// destinationsAllowedFor returns the names of destinations the source's
// allow-list permits, used purely for display.
func destinationsAllowedFor(src *secrets.Source, all []*secrets.Destination) []string {
	out := []string{}
	for _, d := range all {
		if src.AllowsDestination(d.Name) {
			out = append(out, d.Name)
		}
	}
	sort.Strings(out)
	return out
}

func filterDestinations(src *secrets.Source, all []*secrets.Destination) []*secrets.Destination {
	out := make([]*secrets.Destination, 0, len(all))
	for _, d := range all {
		if src.AllowsDestination(d.Name) {
			out = append(out, d)
		}
	}
	return out
}

