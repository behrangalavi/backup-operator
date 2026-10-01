package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"backup-operator/internal/meta"
	"backup-operator/internal/safe"
	"backup-operator/internal/secrets"
	"backup-operator/storage"
)

// destListing is one destination's view of the fleet for a single refresh
// tick: the newest meta.json per target, read with ONE root listing instead
// of one listing per source. Before this, the refresher issued
// sources × destinations List calls against the same backend every tick
// (plus a Get each); now it issues destinations × (1 List + targets Gets),
// which is the floor — every target's meta has to be read once regardless.
//
// err is destination-wide (root listing or session failed → every target on
// this destination is metaError); broken is per target (its newest meta
// could not be read or parsed → metaError for that target only, the
// destination is serving bad data for it).
type destListing struct {
	latest map[string]*meta.MetaFile
	ts     map[string]time.Time
	broken map[string]error
	err    error
}

// lookup maps the listing onto the metaOutcome vocabulary loadLatestMeta
// used, so the aggregation and the destination_failed / Reachable rules are
// unchanged: metaError (unreachable or unreadable), metaEmpty (reachable,
// nothing uploaded for this target yet), metaOK.
func (l *destListing) lookup(target string) (*meta.MetaFile, time.Time, metaOutcome, error) {
	if l == nil {
		return nil, time.Time{}, metaError, fmt.Errorf("destination not listed this tick")
	}
	if l.err != nil {
		return nil, time.Time{}, metaError, l.err
	}
	if err, ok := l.broken[target]; ok {
		return nil, time.Time{}, metaError, err
	}
	m, ok := l.latest[target]
	if !ok {
		return nil, time.Time{}, metaEmpty, nil
	}
	return m, l.ts[target], metaOK, nil
}

// storageFor resolves a destination's storage client: the StorageFor hook
// when wired (tests inject fakes), otherwise the shared pool.
func (r *MetricsRefresher) storageFor(d *secrets.Destination) (storage.Storage, error) {
	if r.StorageFor != nil {
		return r.StorageFor(d)
	}
	r.ensurePool()
	return r.Pool.Get(d)
}

// listDestinations fans out one root listing per destination, bounded by the
// global worker pool and the per-destination slot (one listing per
// destination per tick, so the slot is never contended by this path — it
// stays shared with any other caller of destSlot). Returns one listing per
// destination name; a destination whose client could not even be built gets
// an error listing so every target on it reads as metaError.
func (r *MetricsRefresher) listDestinations(ctx context.Context, dests []*secrets.Destination) map[string]*destListing {
	out := make(map[string]*destListing, len(dests))
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		workers = make(chan struct{}, defaultGlobalConcurrency)
	)
	for _, d := range dests {
		d := d
		slot := r.destSlot(d.Name)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer safe.Goroutine(r.Logger, "metrics-refresh-list-destination", d.Name)
			select {
			case workers <- struct{}{}:
				defer func() { <-workers }()
			case <-ctx.Done():
				return
			}
			select {
			case slot <- struct{}{}:
				defer func() { <-slot }()
			case <-ctx.Done():
				return
			}
			var l *destListing
			st, err := r.storageFor(d)
			if err != nil {
				l = &destListing{err: fmt.Errorf("storage init: %w", err)}
			} else {
				l = loadDestinationListing(ctx, st)
			}
			mu.Lock()
			out[d.Name] = l
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// loadDestinationListing lists the destination root once, picks the newest
// meta.json per target (first path segment, lexically newest timestamp —
// the same selection rule as LatestPerTarget and mostRecentMeta) and reads
// each. Shares one SFTP/FTPS session across the List and every Get.
func loadDestinationListing(ctx context.Context, st storage.Storage) *destListing {
	active := st
	if bs, ok := st.(storage.BatchStorage); ok {
		sess, closer, err := bs.WithSession(ctx)
		if err != nil {
			return &destListing{err: fmt.Errorf("open session: %w", err)}
		}
		defer func() { _ = closer() }()
		active = sess
	}
	objs, err := active.List(ctx, "")
	if err != nil {
		return &destListing{err: fmt.Errorf("list root: %w", err)}
	}
	newest := map[string]string{} // target → newest meta path
	for _, o := range objs {
		if !strings.HasSuffix(o.Path, ".meta.json") {
			continue
		}
		parts := strings.SplitN(o.Path, "/", 2)
		if len(parts) < 2 {
			continue
		}
		if cur, ok := newest[parts[0]]; !ok || o.Path > cur {
			newest[parts[0]] = o.Path
		}
	}
	l := &destListing{
		latest: make(map[string]*meta.MetaFile, len(newest)),
		ts:     make(map[string]time.Time, len(newest)),
		broken: map[string]error{},
	}
	for target, p := range newest {
		m, ts, err := readMeta(ctx, active, p)
		if err != nil {
			l.broken[target] = err
			continue
		}
		l.latest[target] = m
		l.ts[target] = ts
	}
	return l
}

// readMeta fetches and parses one meta.json, returning the run timestamp
// (path-encoded value preferred over storage mtime, which backends bump on
// listing or replicate with skewed clocks).
func readMeta(ctx context.Context, st storage.Storage, p string) (*meta.MetaFile, time.Time, error) {
	rc, err := st.Get(ctx, p)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("get %s: %w", p, err)
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read %s: %w", p, err)
	}
	var m meta.MetaFile
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, time.Time{}, fmt.Errorf("parse %s: %w", p, err)
	}
	m.Path = p
	ts := m.ParsedTimestamp()
	if ts.IsZero() {
		// Legacy name without a parseable timestamp: fall back to the
		// path-less best effort so the run still counts as present.
		ts = time.Now()
	}
	return &m, ts, nil
}
