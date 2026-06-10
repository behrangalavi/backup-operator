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
	"backup-operator/internal/safe"
	"backup-operator/internal/secrets"
	"backup-operator/metrics"

	"github.com/go-logr/logr"
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
	Pool     *StoragePool
	poolOnce sync.Once
}

// ensurePool lazy-builds the pool exactly once. scrubSource fans out to
// concurrent scrubOne goroutines, so an unguarded `if s.Pool == nil` was a
// data race on the lazy path; sync.Once makes it safe and idempotent. A
// pool wired externally (main.go) is left untouched.
func (s *StorageScrubber) ensurePool() {
	s.poolOnce.Do(func() {
		if s.Pool == nil {
			s.Pool = NewStoragePool(s.Logger)
		}
	})
}

const (
	// defaultScrubConcurrency caps the number of full-dump re-streams in
	// flight across the WHOLE tick (not per source). Each scrubOne pulls a
	// complete encrypted dump through sha256, so this is the egress/bandwidth
	// throttle — keep it small.
	defaultScrubConcurrency = 2
	// defaultScrubSourceWorkers caps how many sources are walked concurrently
	// per tick. Sources used to be processed strictly serially, so one source
	// with a slow/large destination blocked every source behind it and a tick
	// on a big fleet could exceed the 24h interval. Walking sources
	// concurrently (while the stream throttle above still bounds actual
	// egress) keeps the tick bounded by the slowest single dump, not the sum.
	defaultScrubSourceWorkers = 8
	defaultScrubInterval      = 24 * time.Hour
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
	s.ensurePool()
	res, err := listBackupSecrets(ctx, s.Client, s.Namespace)
	if err != nil {
		s.Logger.Error(err, "list secrets")
		return
	}
	s.Logger.V(1).Info("scrub tick", "sources", len(res.Sources), "destinations", len(res.Dests))

	// One stream semaphore shared across all sources bounds total concurrent
	// dump re-streams (egress); a separate worker pool bounds how many sources
	// are walked at once so the tick doesn't serialise into a multi-hour run.
	streamSlots := make(chan struct{}, defaultScrubConcurrency)
	workers := make(chan struct{}, defaultScrubSourceWorkers)
	var wg sync.WaitGroup
	for i := range res.Sources {
		src, err := secrets.ParseSource(&res.Sources[i], "")
		if err != nil {
			continue
		}
		wg.Add(1)
		go func(src *secrets.Source) {
			defer wg.Done()
			defer safe.Goroutine(s.Logger, "storage-scrub-source", src.TargetName)
			workers <- struct{}{}
			defer func() { <-workers }()
			s.scrubSource(ctx, src, res.Dests, streamSlots)
		}(src)
	}
	wg.Wait()
}

func (s *StorageScrubber) scrubSource(ctx context.Context, src *secrets.Source, all []*secrets.Destination, streamSlots chan struct{}) {
	allowed := secrets.FilterDestinations(src, all)
	if len(allowed) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, d := range allowed {
		wg.Add(1)
		go func(d *secrets.Destination) {
			defer wg.Done()
			defer safe.Goroutine(s.Logger, "storage-scrub", d.Name)
			// Gate on the shared stream semaphore so total in-flight dump
			// re-streams stay bounded regardless of how many sources are
			// being walked concurrently.
			streamSlots <- struct{}{}
			defer func() { <-streamSlots }()
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
	dumpPath := dumpPathFromMeta(m.Path, m.Compression)
	if dumpPath == "" {
		log.V(1).Info("scrub skipped: cannot derive dump path from meta", "meta", m.Path)
		return
	}

	// Size pre-check: if the meta records a size and we can get the object
	// list cheaply, detect truncation/corruption before downloading the
	// entire dump. This catches the common failure mode (truncated write,
	// partial upload) without any egress cost.
	if m.EncryptedSizeBytes > 0 {
		objs, listErr := st.List(ctx, dumpPath)
		if listErr == nil {
			for _, o := range objs {
				if o.Path == dumpPath && o.Size != m.EncryptedSizeBytes {
					log.Error(fmt.Errorf("size mismatch"), "scrub: storage corruption detected (size pre-check)",
						"path", dumpPath, "want_bytes", m.EncryptedSizeBytes, "got_bytes", o.Size)
					metrics.SetStorageScrubPassed(target, d.Name, false)
					metrics.IncStorageScrubFailed(target, d.Name)
					metrics.SetStorageScrubLastCheck(target, d.Name, time.Now())
					return
				}
			}
		}
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
// suffix derived from the compression algorithm recorded in the meta. Returns
// "" when the meta path does not match the expected suffix.
func dumpPathFromMeta(metaPath, compression string) string {
	const metaSuffix = ".meta.json"
	if !strings.HasSuffix(metaPath, metaSuffix) {
		return ""
	}
	base := strings.TrimSuffix(metaPath, metaSuffix)
	return base + "." + labels.DumpSuffix(compression)
}

