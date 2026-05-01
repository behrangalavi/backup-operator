package controllers

import (
	"context"
	"encoding/json"
	"io"
	"path"
	"sync"
	"time"

	"backup-operator/internal/labels"
	"backup-operator/internal/meta"
	"backup-operator/internal/secrets"
	"backup-operator/metrics"
	"backup-operator/storage"
	storageFactory "backup-operator/storage/factory"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const defaultRefreshConcurrency = 4

// MetricsRefresher periodically rebuilds the operator's Prometheus gauges from
// the latest meta.json sidecar found at each destination. Worker pods are
// short-lived so Prometheus cannot scrape them in time; this aggregator is the
// long-lived process that turns "what storage knows" back into live metrics.
type MetricsRefresher struct {
	Client    client.Client
	Logger    logr.Logger
	Namespace string
	Interval  time.Duration

	// trackedTargets remembers which targets we exposed last refresh, so we
	// can drop their series when a Source Secret disappears. Without this,
	// a deleted source would leave stale metrics around indefinitely.
	mu             sync.Mutex
	trackedTargets map[string]bool
}

// Start runs the refresh loop until ctx is cancelled. It satisfies
// manager.Runnable so the controller-runtime Manager owns its lifecycle.
func (r *MetricsRefresher) Start(ctx context.Context) error {
	if r.Interval <= 0 {
		r.Interval = 30 * time.Second
	}
	r.Logger.Info("starting metrics refresher", "interval", r.Interval, "namespace", r.Namespace)
	r.refresh(ctx)

	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			r.refresh(ctx)
		}
	}
}

// NeedLeaderElection ensures only the lead operator pulls from storage, so
// replicas don't multiply the read load against destinations.
func (r *MetricsRefresher) NeedLeaderElection() bool { return true }

func (r *MetricsRefresher) refresh(ctx context.Context) {
	sources, dests, err := r.listSecrets(ctx)
	if err != nil {
		r.Logger.Error(err, "list secrets")
		return
	}
	r.Logger.V(1).Info("refresh tick", "sources", len(sources), "destinations", len(dests))

	current := make(map[string]bool, len(sources))
	for _, s := range sources {
		src, err := secrets.ParseSource(&s, "")
		if err != nil {
			r.Logger.V(1).Info("skipping invalid source", "secret", s.Name, "err", err.Error())
			continue
		}
		current[src.TargetName] = true
		r.refreshSource(ctx, src, dests)
	}

	r.mu.Lock()
	for prev := range r.trackedTargets {
		if !current[prev] {
			metrics.DeleteTargetMetrics(prev)
		}
	}
	r.trackedTargets = current
	r.mu.Unlock()
}

func (r *MetricsRefresher) listSecrets(ctx context.Context) ([]corev1.Secret, []*secrets.Destination, error) {
	var srcList corev1.SecretList
	srcOpts := []client.ListOption{client.MatchingLabels{labels.LabelRole: labels.RoleSource}}
	if r.Namespace != "" {
		srcOpts = append(srcOpts, client.InNamespace(r.Namespace))
	}
	if err := r.Client.List(ctx, &srcList, srcOpts...); err != nil {
		return nil, nil, err
	}

	var destList corev1.SecretList
	destOpts := []client.ListOption{client.MatchingLabels{labels.LabelRole: labels.RoleDestination}}
	if r.Namespace != "" {
		destOpts = append(destOpts, client.InNamespace(r.Namespace))
	}
	if err := r.Client.List(ctx, &destList, destOpts...); err != nil {
		return nil, nil, err
	}

	dests := make([]*secrets.Destination, 0, len(destList.Items))
	for i := range destList.Items {
		d, err := secrets.ParseDestination(&destList.Items[i])
		if err != nil {
			r.Logger.V(1).Info("skipping invalid destination", "secret", destList.Items[i].Name, "err", err.Error())
			continue
		}
		dests = append(dests, d)
	}
	return srcList.Items, dests, nil
}

func (r *MetricsRefresher) refreshSource(ctx context.Context, src *secrets.Source, all []*secrets.Destination) {
	allowed := secrets.FilterDestinations(src, all)

	// We track two independent "best" metas across destinations:
	//   - newest:    dictates last_run_status / last_run_anomalies / size_change_ratio
	//                even if it represents a failed run
	//   - success:   dictates dump_size, table_count, last_success_timestamp —
	//                fields that only make sense when a real artifact exists
	var (
		newest, success        *meta.MetaFile
		newestTS, successTS    time.Time
		resultMu               sync.Mutex
		wg                     sync.WaitGroup
	)
	sem := make(chan struct{}, defaultRefreshConcurrency)
	for _, d := range allowed {
		wg.Add(1)
		go func(d *secrets.Destination) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			st, err := storageFactory.NewStorage(d.StorageType, d.Name, d.Data, r.Logger)
			if err != nil {
				r.Logger.V(1).Info("storage init failed; treating destination as failing",
					"target", src.TargetName, "destination", d.Name, "err", err.Error())
				metrics.SetDestinationFailed(src.TargetName, d.Name, true)
				return
			}
			m, ts, found := loadLatestMeta(ctx, st, src.TargetName)
			if !found {
				return
			}
			metrics.SetDestinationFailed(src.TargetName, d.Name, false)
			if !m.IsFailure() {
				metrics.SetLastSuccess(src.TargetName, d.Name, ts)
			}

			resultMu.Lock()
			if newest == nil || ts.After(newestTS) {
				newest = m
				newestTS = ts
			}
			if !m.IsFailure() && (success == nil || ts.After(successTS)) {
				success = m
				successTS = ts
			}
			resultMu.Unlock()
		}(d)
	}
	wg.Wait()

	if newest == nil {
		// No data anywhere yet — leave gauges absent. lastRunStatus only
		// becomes meaningful once at least one run has uploaded a meta.
		return
	}

	metrics.SetLastRunStatus(src.TargetName, !newest.IsFailure())
	if newest.Report != nil {
		if newest.Report.SizeChangeRatio > 0 {
			metrics.SetDumpSizeChangeRatio(src.TargetName, newest.Report.SizeChangeRatio)
		}
		metrics.SetSchemaChanged(src.TargetName, newest.Report.SchemaChanged)
		metrics.SetLastRunAnomalies(src.TargetName, len(newest.Report.Anomalies))
	} else {
		// A failed run won't have a report. Keep these gauges sticky on their
		// last known good values rather than zeroing them — a transient
		// failure should not silence schema/size alerts.
		metrics.SetLastRunAnomalies(src.TargetName, 0)
	}

	if success != nil {
		metrics.SetDumpSize(src.TargetName, success.EncryptedSizeBytes)
		if success.Stats != nil {
			metrics.SetTableCount(src.TargetName, len(success.Stats.Tables))
			for _, t := range success.Stats.Tables {
				metrics.SetTableRowCount(src.TargetName, t.Name, t.RowCount)
			}
		}
	}
}

// loadLatestMeta fetches and parses the most recent *.meta.json under the
// given target prefix. Returns (nil, zero-time, false) if storage cannot be
// listed, no meta exists, or the latest one cannot be parsed.
func loadLatestMeta(ctx context.Context, st storage.Storage, target string) (*meta.MetaFile, time.Time, bool) {
	objs, err := st.List(ctx, target+"/")
	if err != nil || len(objs) == 0 {
		return nil, time.Time{}, false
	}
	latest := mostRecentMeta(objs)
	if latest.Path == "" {
		return nil, time.Time{}, false
	}
	rc, err := st.Get(ctx, latest.Path)
	if err != nil {
		return nil, time.Time{}, false
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return nil, time.Time{}, false
	}
	var m meta.MetaFile
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, time.Time{}, false
	}
	ts := latest.LastModified
	if parsed := m.ParsedTimestamp(); !parsed.IsZero() {
		// Prefer the timestamp baked into the meta payload over the storage
		// LastModified, since some backends update mtime on listing or
		// replicate with skewed clocks.
		ts = parsed
	}
	return &m, ts, true
}

func mostRecentMeta(objs []storage.Object) storage.Object {
	var latest storage.Object
	for _, o := range objs {
		if path.Ext(o.Path) != ".json" {
			continue
		}
		if o.LastModified.After(latest.LastModified) {
			latest = o
		}
	}
	return latest
}
