package ui

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"backup-operator/internal/labels"
	"backup-operator/internal/meta"
	"backup-operator/internal/safe"
	"backup-operator/internal/secrets"
	"backup-operator/metrics"
	"backup-operator/storage"

	dto "github.com/prometheus/client_model/go"
)

// --- Destination storage stats ---

type destStorageStats struct {
	Name           string `json:"name"`
	StorageType    string `json:"storageType"`
	TotalFiles     int    `json:"totalFiles"`
	TotalSizeBytes int64  `json:"totalSizeBytes"`
	BackupCount    int    `json:"backupCount"`
	MetaCount      int    `json:"metaCount"`
	OldestBackup   string `json:"oldestBackup,omitempty"`
	NewestBackup   string `json:"newestBackup,omitempty"`
	Error          string `json:"error,omitempty"`
}

func (s *Server) handleAPIDestinationStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "GET required")
		return
	}

	dests, err := s.data.listDestinations(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}

	results := make([]destStorageStats, len(dests))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range dests {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			dest := dests[idx]
			defer safe.Goroutine(s.cfg.Logger, "destination-stats", dest.Name)
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-r.Context().Done():
				return
			}
			results[idx] = s.destinationStats(r.Context(), dest)
		}(i)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, results)
}

// destinationStats lists one destination root and summarises it. The 8 s
// timeout is deliberate: this is a live UI probe, not the backup run — a
// broken destination must surface fast or the dashboard blocks behind it on
// every page-load and SSE refresh.
func (s *Server) destinationStats(parent context.Context, dest *secrets.Destination) destStorageStats {
	stat := destStorageStats{Name: dest.Name, StorageType: dest.StorageType}
	st, err := s.storageFor(dest, "stats")
	if err != nil {
		stat.Error = "storage init failed"
		return stat
	}
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	objs, err := st.List(ctx, "")
	if err != nil {
		stat.Error = sanitizeStorageError(err)
		return stat
	}
	summarizeObjects(&stat, objs)
	return stat
}

// summarizeObjects fills counts, total bytes and the oldest/newest meta
// timestamp of a destination listing.
func summarizeObjects(stat *destStorageStats, objs []storage.Object) {
	stat.TotalFiles = len(objs)
	var oldest, newest string
	for _, o := range objs {
		stat.TotalSizeBytes += o.Size
		switch {
		case strings.HasSuffix(o.Path, ".meta.json"):
			stat.MetaCount++
			ts := extractTimestamp(o.Path)
			if ts == "" {
				continue
			}
			if oldest == "" || ts < oldest {
				oldest = ts
			}
			if newest == "" || ts > newest {
				newest = ts
			}
		case labels.IsDumpSuffix(o.Path):
			stat.BackupCount++
		}
	}
	stat.OldestBackup = oldest
	stat.NewestBackup = newest
}

// extractTimestamp pulls a compact timestamp from a meta path like
// "target/2026/05/01/dump-20260501T020000Z.meta.json" → "20260501T020000Z"
func extractTimestamp(p string) string {
	base := p
	if idx := strings.LastIndex(p, "/"); idx >= 0 {
		base = p[idx+1:]
	}
	base = strings.TrimPrefix(base, "dump-")
	base = strings.TrimSuffix(base, ".meta.json")
	if len(base) == 16 && base[8] == 'T' && base[15] == 'Z' {
		return base
	}
	return ""
}

// --- Destination health matrix ---

type destHealthEntry struct {
	Target      string `json:"target"`
	Destination string `json:"destination"`
	StorageType string `json:"storageType"`
	HasBackup   bool   `json:"hasBackup"`
	LatestRun   string `json:"latestRun,omitempty"`
	Status      string `json:"status"` // "ok", "failed", "missing", "unreachable"
	Error       string `json:"error,omitempty"`

	// Scrub fields are populated when STORAGE_SCRUB_ENABLED=true and at least
	// one scrub has run for this pair. ScrubStatus is "" when no scrub data
	// is available — the UI can then either hide the column or render
	// "disabled" depending on whether the operator advertises scrub support.
	ScrubStatus      string `json:"scrubStatus,omitempty"`      // "ok" | "failed"
	ScrubLastCheck   int64  `json:"scrubLastCheck,omitempty"`   // unix seconds
	ScrubFailedTotal int64  `json:"scrubFailedTotal,omitempty"` // cumulative failures
}

func (s *Server) handleAPIDestinationHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "GET required")
		return
	}

	sources, err := s.data.listTargets(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}

	dests, err := s.data.listDestinations(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}

	destLatest := s.latestPerDestination(r, dests)

	var entries []destHealthEntry
	for _, src := range sources {
		for _, dest := range dests {
			if !destAllowed(src.Destinations, dest.Name) {
				continue
			}
			lr, ok := destLatest[dest.Name]
			entries = append(entries, healthEntry(src, dest, lr, ok))
		}
	}

	// Decorate with scrub status from our own metric registry. Done last so
	// it's a no-op when STORAGE_SCRUB_ENABLED is off (the gauges are simply
	// absent and the lookup returns zero values).
	scrub := collectScrubStatus()
	for i := range entries {
		key := entries[i].Target + "\x00" + entries[i].Destination
		if s, ok := scrub[key]; ok {
			entries[i].ScrubStatus = s.status
			entries[i].ScrubLastCheck = s.lastCheck
			entries[i].ScrubFailedTotal = s.failedTotal
		}
	}
	writeJSON(w, http.StatusOK, entries)
}

// latestLookup is one destination's "latest meta per target" probe result.
type latestLookup struct {
	latest map[string]*meta.MetaFile
	err    error
}

// latestPerDestination probes every destination for its newest meta per
// target, four at a time, each under the short UI-probe timeout.
func (s *Server) latestPerDestination(r *http.Request, dests []*secrets.Destination) map[string]latestLookup {
	destLatest := make(map[string]latestLookup, len(dests))
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, 4)
	for _, dest := range dests {
		wg.Add(1)
		go func(d *secrets.Destination) {
			defer wg.Done()
			defer safe.Goroutine(s.cfg.Logger, "destination-health", d.Name)
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-r.Context().Done():
				return
			}
			res := latestLookup{}
			if st, err := s.storageFor(d, "health"); err != nil {
				res.err = err
			} else {
				ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
				defer cancel()
				res.latest, res.err = meta.LatestPerTarget(ctx, st)
			}
			mu.Lock()
			destLatest[d.Name] = res
			mu.Unlock()
		}(dest)
	}
	wg.Wait()
	return destLatest
}

// destAllowed mirrors secrets.Source.AllowsDestination on the summary's
// name list: an empty allow-list permits every destination.
func destAllowed(allowed []string, name string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if a == name {
			return true
		}
	}
	return false
}

// healthEntry classifies one (target, destination) pair: unreachable (no
// probe result or probe error), missing (no run for this target), failed or ok.
func healthEntry(src targetSummary, dest *secrets.Destination, lr latestLookup, probed bool) destHealthEntry {
	entry := destHealthEntry{Target: src.Name, Destination: dest.Name, StorageType: dest.StorageType}
	if !probed || lr.err != nil {
		entry.Status = "unreachable"
		if lr.err != nil {
			entry.Error = sanitizeStorageError(lr.err)
		}
		return entry
	}
	m, exists := lr.latest[src.Name]
	if !exists {
		entry.Status = "missing"
		return entry
	}
	entry.HasBackup = true
	entry.LatestRun = m.Timestamp
	if m.IsFailure() {
		entry.Status = "failed"
		entry.Error = m.Error
	} else {
		entry.Status = "ok"
	}
	return entry
}

type scrubInfo struct {
	status      string // "ok" | "failed"
	lastCheck   int64
	failedTotal int64
}

// collectScrubStatus walks the operator's own metric registry and indexes
// scrub gauges by (target, destination). Used to decorate destination-health
// entries without an extra storage round-trip — the scrub already touched
// storage on its own schedule, so there is no point re-doing it per UI hit.
func collectScrubStatus() map[string]scrubInfo {
	out := map[string]scrubInfo{}
	g := metrics.Gatherer()
	if g == nil {
		return out
	}
	families, err := g.Gather()
	if err != nil {
		return out
	}
	getOrInit := func(target, dest string) scrubInfo {
		k := target + "\x00" + dest
		return out[k]
	}
	put := func(target, dest string, info scrubInfo) {
		out[target+"\x00"+dest] = info
	}
	for _, fam := range families {
		switch fam.GetName() {
		case metrics.NameStorageScrubPassed:
			for _, m := range fam.Metric {
				target, dest := scrubLabels(m.Label)
				if target == "" {
					continue
				}
				info := getOrInit(target, dest)
				if m.Gauge != nil && m.Gauge.GetValue() == 1 {
					info.status = "ok"
				} else {
					info.status = "failed"
				}
				put(target, dest, info)
			}
		case metrics.NameStorageScrubLastCheckTimestampSeconds:
			for _, m := range fam.Metric {
				target, dest := scrubLabels(m.Label)
				if target == "" {
					continue
				}
				info := getOrInit(target, dest)
				if m.Gauge != nil {
					info.lastCheck = int64(m.Gauge.GetValue())
				}
				put(target, dest, info)
			}
		case metrics.NameStorageScrubFailedTotal:
			for _, m := range fam.Metric {
				target, dest := scrubLabels(m.Label)
				if target == "" {
					continue
				}
				info := getOrInit(target, dest)
				if m.Counter != nil {
					info.failedTotal = int64(m.Counter.GetValue())
				}
				put(target, dest, info)
			}
		}
	}
	return out
}

func scrubLabels(pairs []*dto.LabelPair) (target, dest string) {
	for _, p := range pairs {
		switch p.GetName() {
		case "target":
			target = p.GetValue()
		case "destination":
			dest = p.GetValue()
		}
	}
	return
}

// --- Backup consistency check ---

type consistencyIssue struct {
	Target      string   `json:"target"`
	Timestamp   string   `json:"timestamp"`
	PresentIn   []string `json:"presentIn"`
	MissingFrom []string `json:"missingFrom"`
}

func (s *Server) handleAPIConsistencyCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "GET required")
		return
	}

	sources, err := s.data.listTargets(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}

	dests, err := s.data.listDestinations(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}

	allDestRuns := s.collectDestRuns(r, dests)

	var issues []consistencyIssue
	for _, src := range sources {
		var relevant []destRuns
		for _, dr := range allDestRuns {
			if destAllowed(src.Destinations, dr.name) {
				relevant = append(relevant, dr)
			}
		}
		if len(relevant) < 2 {
			continue
		}
		issues = append(issues, consistencyIssuesFor(src.Name, relevant)...)
	}
	// Deterministic newest-first ordering. The map-range above produced
	// random order; pagination (slice(0,20) in the UI) was unstable
	// between refreshes, so the user kept seeing different rows for the
	// same data. Sort once here, never twice.
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Timestamp != issues[j].Timestamp {
			return issues[i].Timestamp > issues[j].Timestamp
		}
		return issues[i].Target < issues[j].Target
	})
	writeJSON(w, http.StatusOK, issues)
}

// destRuns is the set of "<target>@<timestamp>" meta keys one destination holds.
type destRuns struct {
	name       string
	timestamps map[string]bool
}

// collectDestRuns lists every destination root (four at a time, short
// timeout) and indexes the meta sidecars by target and timestamp. A
// destination that cannot be listed simply contributes no runs.
func (s *Server) collectDestRuns(r *http.Request, dests []*secrets.Destination) []destRuns {
	all := make([]destRuns, len(dests))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, dest := range dests {
		all[i] = destRuns{name: dest.Name, timestamps: make(map[string]bool)}
		wg.Add(1)
		go func(idx int, d *secrets.Destination) {
			defer wg.Done()
			defer safe.Goroutine(s.cfg.Logger, "consistency-check", d.Name)
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-r.Context().Done():
				return
			}
			st, err := s.storageFor(d, "consistency")
			if err != nil {
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
			defer cancel()
			objs, err := st.List(ctx, "")
			if err != nil {
				return
			}
			for _, o := range objs {
				if !strings.HasSuffix(o.Path, ".meta.json") {
					continue
				}
				target := strings.SplitN(o.Path, "/", 2)[0]
				if ts := extractTimestamp(o.Path); ts != "" {
					all[idx].timestamps[target+"@"+ts] = true
				}
			}
		}(i, dest)
	}
	wg.Wait()
	return all
}

// consistencyIssuesFor reports timestamps of one target that some relevant
// destinations hold and others do not. A destination that has no run for
// the target at all, or whose earliest run is newer than the timestamp (it
// was onboarded later), is not counted as "missing" — that is a different
// problem class and would be noise after every destination addition.
func consistencyIssuesFor(target string, relevant []destRuns) []consistencyIssue {
	prefix := target + "@"
	earliestPerDest := make(map[string]string, len(relevant))
	allTS := map[string]bool{}
	for _, dr := range relevant {
		for key := range dr.timestamps {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			ts := strings.TrimPrefix(key, prefix)
			allTS[ts] = true
			if cur, ok := earliestPerDest[dr.name]; !ok || ts < cur {
				earliestPerDest[dr.name] = ts
			}
		}
	}
	var issues []consistencyIssue
	for ts := range allTS {
		var present, missing []string
		for _, dr := range relevant {
			if dr.timestamps[prefix+ts] {
				present = append(present, dr.name)
				continue
			}
			if earliest, has := earliestPerDest[dr.name]; has && ts >= earliest {
				missing = append(missing, dr.name)
			}
		}
		if len(missing) > 0 && len(present) > 0 {
			issues = append(issues, consistencyIssue{Target: target, Timestamp: ts, PresentIn: present, MissingFrom: missing})
		}
	}
	return issues
}
