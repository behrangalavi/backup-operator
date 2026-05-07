package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"backup-operator/internal/labels"
	"backup-operator/internal/secrets"
	"backup-operator/metrics"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// StorageScrubber periodically re-hashes the most recent dump per
// source/destination and compares the result with the SHA256 recorded in the
// matching meta.json. It detects silent storage corruption — the failure mode
// where bytes rot in the bucket but the operator never re-reads them, so
// nobody notices until restore.
//
// Trade-off: each scrub re-streams a full encrypted dump from storage. At
// scale, that is real egress. The default interval is 24h and the feature is
// opt-in via STORAGE_SCRUB_ENABLED=true so existing deployments don't grow a
// surprise bandwidth bill on upgrade.
type StorageScrubber struct {
	Client    client.Client
	Logger    logr.Logger
	Namespace string
	Interval  time.Duration

	// Pool is the per-destination storage cache. Optional; lazy-built when
	// nil. Production wiring shares one pool with MetricsRefresher.
	Pool *StoragePool
}

const (
	defaultScrubConcurrency = 2
	defaultScrubInterval    = 24 * time.Hour
)

// Start runs the scrub loop until ctx is cancelled. Satisfies manager.Runnable.
func (s *StorageScrubber) Start(ctx context.Context) error {
	if s.Interval <= 0 {
		s.Interval = defaultScrubInterval
	}
	s.Logger.Info("starting storage scrubber", "interval", s.Interval, "namespace", s.Namespace)
	// First scrub on a short delay so a freshly started operator doesn't
	// immediately hammer storage; gives the metrics refresher time to populate
	// state for the dashboard first.
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(time.Minute):
	}
	s.scrub(ctx)

	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.scrub(ctx)
		}
	}
}

// NeedLeaderElection ensures only the lead operator pulls bytes for scrubbing
// — replicas would otherwise multiply the egress cost.
func (s *StorageScrubber) NeedLeaderElection() bool { return true }

func (s *StorageScrubber) scrub(ctx context.Context) {
	sources, dests, err := s.listSecrets(ctx)
	if err != nil {
		s.Logger.Error(err, "list secrets")
		return
	}
	s.Logger.V(1).Info("scrub tick", "sources", len(sources), "destinations", len(dests))

	for i := range sources {
		src, err := secrets.ParseSource(&sources[i], "")
		if err != nil {
			continue
		}
		s.scrubSource(ctx, src, dests)
	}
}

func (s *StorageScrubber) listSecrets(ctx context.Context) ([]corev1.Secret, []*secrets.Destination, error) {
	var srcList corev1.SecretList
	srcOpts := []client.ListOption{client.MatchingLabels{labels.LabelRole: labels.RoleSource}}
	if s.Namespace != "" {
		srcOpts = append(srcOpts, client.InNamespace(s.Namespace))
	}
	if err := s.Client.List(ctx, &srcList, srcOpts...); err != nil {
		return nil, nil, err
	}

	var destList corev1.SecretList
	destOpts := []client.ListOption{client.MatchingLabels{labels.LabelRole: labels.RoleDestination}}
	if s.Namespace != "" {
		destOpts = append(destOpts, client.InNamespace(s.Namespace))
	}
	if err := s.Client.List(ctx, &destList, destOpts...); err != nil {
		return nil, nil, err
	}

	dests := make([]*secrets.Destination, 0, len(destList.Items))
	for i := range destList.Items {
		d, err := secrets.ParseDestination(&destList.Items[i])
		if err != nil {
			continue
		}
		dests = append(dests, d)
	}
	return srcList.Items, dests, nil
}

func (s *StorageScrubber) scrubSource(ctx context.Context, src *secrets.Source, all []*secrets.Destination) {
	allowed := secrets.FilterDestinations(src, all)
	if len(allowed) == 0 {
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, defaultScrubConcurrency)
	for _, d := range allowed {
		wg.Add(1)
		go func(d *secrets.Destination) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.scrubOne(ctx, src.TargetName, d)
		}(d)
	}
	wg.Wait()
}

// scrubOne fetches the latest meta.json for the target on a single
// destination, locates the matching encrypted dump, re-hashes it, and
// compares with the meta-recorded SHA256. The result populates per-pair
// gauges/counters so Alertmanager can notice corruption.
func (s *StorageScrubber) scrubOne(ctx context.Context, target string, d *secrets.Destination) {
	log := s.Logger.WithValues("target", target, "destination", d.Name)
	if s.Pool == nil {
		s.Pool = NewStoragePool(s.Logger)
	}
	st, err := s.Pool.Get(d)
	if err != nil {
		log.V(1).Info("scrub skipped: storage init failed", "err", err.Error())
		return
	}
	m, _, found := loadLatestMeta(ctx, st, target)
	if !found || m == nil {
		log.V(1).Info("scrub skipped: no meta available")
		return
	}
	if m.IsFailure() {
		// Failure-meta has no dump alongside; nothing to scrub.
		return
	}
	if m.SHA256 == "" {
		// Older meta files predate sha256 recording. We can't verify, so we
		// don't fail the run — but we also don't claim success.
		log.V(1).Info("scrub skipped: meta has no sha256 field (legacy run)")
		return
	}
	dumpPath := dumpPathFromMeta(m.Path)
	if dumpPath == "" {
		log.V(1).Info("scrub skipped: cannot derive dump path from meta", "meta", m.Path)
		return
	}

	rc, err := st.Get(ctx, dumpPath)
	if err != nil {
		log.Info("scrub: dump fetch failed", "path", dumpPath, "err", err.Error())
		metrics.SetStorageScrubPassed(target, d.Name, false)
		metrics.IncStorageScrubFailed(target, d.Name)
		metrics.SetStorageScrubLastCheck(target, d.Name, time.Now())
		return
	}
	defer func() { _ = rc.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		log.Info("scrub: stream copy failed", "path", dumpPath, "err", err.Error())
		metrics.SetStorageScrubPassed(target, d.Name, false)
		metrics.IncStorageScrubFailed(target, d.Name)
		metrics.SetStorageScrubLastCheck(target, d.Name, time.Now())
		return
	}
	got := hex.EncodeToString(h.Sum(nil))
	metrics.SetStorageScrubLastCheck(target, d.Name, time.Now())
	if got != m.SHA256 {
		log.Error(fmt.Errorf("checksum mismatch"), "scrub: storage corruption detected",
			"path", dumpPath, "want", m.SHA256, "got", got)
		metrics.SetStorageScrubPassed(target, d.Name, false)
		metrics.IncStorageScrubFailed(target, d.Name)
		return
	}
	metrics.SetStorageScrubPassed(target, d.Name, true)
	log.V(1).Info("scrub: passed", "path", dumpPath)
}

// dumpPathFromMeta replaces the trailing ".meta.json" with the encrypted dump
// suffix (".sql.gz.age") used by the pipeline. Returns "" when the meta path
// does not match the expected suffix — keeps the scrubber robust against
// future format changes rather than silently scrubbing the wrong object.
func dumpPathFromMeta(metaPath string) string {
	const metaSuffix = ".meta.json"
	const dumpSuffix = ".sql.gz.age"
	if !strings.HasSuffix(metaPath, metaSuffix) {
		return ""
	}
	base := strings.TrimSuffix(metaPath, metaSuffix)
	return base + dumpSuffix
}

