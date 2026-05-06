// Package meta exposes the on-disk shape of the unencrypted sidecar
// `<dump>.meta.json` so consumers (UI, future tooling) can browse run
// history without depending on the worker's pipeline package.
//
// The pipeline writes these files; this package only reads. The struct
// mirrors the JSON shape the pipeline emits — keep the JSON tags in sync
// if pipeline.metaFile evolves.
package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"backup-operator/analyzer"
	"backup-operator/dumper"
	"backup-operator/storage"
)

// Run status values used in MetaFile.Status. Empty Status is treated as
// StatusSuccess for backwards compatibility with metas written before the
// failure-meta feature existed.
const (
	StatusSuccess = "success"
	StatusFailed  = "failed"
)

// DestinationResult records the upload outcome for a single destination.
type DestinationResult struct {
	Name        string `json:"name"`
	StorageType string `json:"storageType"`
	Status      string `json:"status"` // "success" or "failed"
	Error       string `json:"error,omitempty"`
}

// RetentionResult records the outcome of a single destination's retention
// sweep. Only the pre-upload sweep is captured here — the post-upload sweep
// runs after the meta is in storage, so its results would arrive too late
// to land in the same artifact. That is acceptable: the pre-upload sweep is
// the load-bearing path (it frees space for the new dump), the post-upload
// sweep just trims one extra cohort if the new artifact pushed the count
// above threshold.
//
// Status mirrors the DestinationResult constants ("success" / "failed").
// DeletedDumps / DeletedMetas / DeletedOther break down what was pruned so
// dashboards can answer "are we trimming actual data or just stale meta
// sidecars".
type RetentionResult struct {
	Name          string `json:"name"`
	Status        string `json:"status"`
	DeletedDumps  int    `json:"deletedDumps,omitempty"`
	DeletedMetas  int    `json:"deletedMetas,omitempty"`
	DeletedOther  int    `json:"deletedOther,omitempty"`
	Error         string `json:"error,omitempty"`
}

// Verification verdict constants.
const (
	VerificationMatch    = "match"
	VerificationMismatch = "mismatch"
	VerificationPartial  = "partial"
	VerificationSkipped  = "skipped"
)

// DumpVerification records the integrity verification result for a backup run.
// Pre-dump stats are collected before the dump starts, post-dump stats after
// it finishes. Dump row counts are parsed from the dump stream itself. The
// Verdict summarises whether the three sources agree.
type DumpVerification struct {
	Verdict  string              `json:"verdict"`
	Summary  string              `json:"summary"`
	PreStats *dumper.Stats       `json:"preStats,omitempty"`
	PostStats *dumper.Stats      `json:"postStats,omitempty"`
	DumpRowCounts map[string]int64 `json:"dumpRowCounts,omitempty"`
	Tables   []TableVerification `json:"tables,omitempty"`

	// LooksEmpty is true when the dump appears to contain no data despite the
	// source DB having rows. Set when dump row counting is available, the
	// pre-dump stats show > 0 total rows across user tables, and the dump
	// itself shows 0 rows. The classic "mysqldump succeeded but only emitted
	// DDL because the user lacked SELECT" failure mode.
	LooksEmpty bool `json:"looksEmpty,omitempty"`
}

// TableVerification records per-table row counts from three sources.
type TableVerification struct {
	Name          string `json:"name"`
	PreDumpRows   int64  `json:"preDumpRows"`
	PostDumpRows  int64  `json:"postDumpRows"`
	DumpRows      int64  `json:"dumpRows"`
	Verdict       string `json:"verdict"`
	Detail        string `json:"detail,omitempty"`
}

// RestoreVerificationResult records the outcome of a single
// restore-verification phase: did the just-uploaded encrypted artifact
// successfully decrypt and parse? Distinct from DumpVerification (which
// runs DURING the dump and compares pre/post stats); this runs AFTER the
// dump landed in storage and proves the round-trip.
//
// The Mode field maps 1:1 to labels.RestoreVerification* constants so
// dashboards can group by capability level. Phase 1 only emits Mode =
// "stream-validate"; later modes (schema-only, sample, full) plug into
// the same struct.
type RestoreVerificationResult struct {
	Mode                          string    `json:"mode"`
	Verdict                       string    `json:"verdict"` // re-uses Verification* constants
	Summary                       string    `json:"summary,omitempty"`
	Error                         string    `json:"error,omitempty"`
	StartedAt                     time.Time `json:"startedAt,omitempty"`
	CompletedAt                   time.Time `json:"completedAt,omitempty"`
	DurationSeconds               float64   `json:"durationSeconds,omitempty"`
	EphemeralRecipientFingerprint string    `json:"ephemeralRecipientFingerprint,omitempty"`
}

// MetaFile is the deserialised representation of a `dump-<ts>.meta.json`.
//
// A failure run writes a meta file too, with Status="failed" and no dump
// alongside it — that's how the UI surfaces failures that never produced
// a dump (e.g. wrong DB password, unreachable host).
type MetaFile struct {
	Target             string           `json:"target"`
	Timestamp          string           `json:"timestamp"`
	DBType             string           `json:"dbType"`
	Status             string           `json:"status,omitempty"`
	Error              string           `json:"error,omitempty"`
	Phase              string           `json:"phase,omitempty"`
	EncryptedSizeBytes int64            `json:"encryptedSizeBytes,omitempty"`
	SHA256             string           `json:"sha256,omitempty"`
	// SchemaChangedAt is the timestamp of the most recent run where the
	// schema fingerprint differed from the prior run. Carried forward from
	// the previous meta when the schema is unchanged, so any single meta
	// alone tells you "schema was last touched at ...". Lets PromQL queries
	// like `time() - last_change_timestamp > 86400 * 180` flag backups whose
	// schema is so old the application has likely diverged.
	SchemaChangedAt    time.Time        `json:"schemaChangedAt,omitempty"`
	// CompletedAt and DurationSeconds describe the run's wall-clock duration.
	// Timestamp is the run's start; CompletedAt is when the meta was written
	// (after dump + fan-out + retention). DurationSeconds == CompletedAt -
	// Timestamp's parsed value, persisted explicitly so callers don't have to
	// recompute and so legacy metas (where these fields are absent) are
	// distinguishable from genuinely-zero durations.
	CompletedAt        time.Time        `json:"completedAt,omitempty"`
	DurationSeconds    float64          `json:"durationSeconds,omitempty"`
	Stats              *dumper.Stats    `json:"stats,omitempty"`
	// StatsError records the sanitized message from a failed pre-dump
	// CollectStats call. Without it, a permission / connect failure on
	// pg_stat_user_tables (or its MySQL / Mongo equivalents) leaves
	// Stats==nil with no operator-visible signal — the surface symptom
	// is a "skipped" restore-verification verdict (no preTables to
	// compare). Empty when stats collection succeeded or the analyzer
	// is disabled by annotation.
	StatsError         string           `json:"statsError,omitempty"`
	Report             *analyzer.Report `json:"report,omitempty"`
	Verification       *DumpVerification `json:"verification,omitempty"`
	// RestoreVerification is set when a restore-verifier ran for this run.
	// Absent when verification was off or not yet due. Distinct field from
	// Verification (dump-stream check) — both can coexist on the same run.
	RestoreVerification *RestoreVerificationResult `json:"restoreVerification,omitempty"`
	Destinations       []DestinationResult `json:"destinations,omitempty"`
	// Retention captures the pre-upload retention sweep's outcome per
	// destination. Empty when retention is disabled (Days==0) or the run
	// failed before retention ran. See RetentionResult for details.
	Retention          []RetentionResult `json:"retention,omitempty"`

	// Path within the destination, populated when fetched via List+Get so
	// callers can deep-link or correlate to the encrypted dump alongside.
	Path string `json:"-"`

	// SourceDestination is set at read time to indicate which destination
	// this meta was fetched from. Not persisted in JSON.
	SourceDestination string `json:"-"`
}

// IsFailure reports whether the meta represents a failed run. Empty Status
// counts as success so legacy metas still render correctly.
func (m *MetaFile) IsFailure() bool { return m.Status == StatusFailed }

// ParsedTimestamp returns the file's timestamp as time.Time, or zero if
// the timestamp could not be parsed.
func (m *MetaFile) ParsedTimestamp() time.Time {
	t, err := time.Parse("20060102T150405Z", m.Timestamp)
	if err != nil {
		return time.Time{}
	}
	return t
}

// List enumerates every meta.json under target/ in the storage and
// returns them parsed and sorted newest-first. Errors on individual
// files are skipped — one corrupt meta should not blank the UI.
func List(ctx context.Context, st storage.Storage, target string) ([]*MetaFile, error) {
	objs, err := st.List(ctx, target+"/")
	if err != nil {
		return nil, fmt.Errorf("list %q: %w", target, err)
	}
	destName := st.Name()
	out := make([]*MetaFile, 0, len(objs))
	for _, o := range objs {
		if path.Ext(o.Path) != ".json" || !strings.HasSuffix(o.Path, ".meta.json") {
			continue
		}
		m, err := fetchOne(ctx, st, o.Path)
		if err != nil {
			continue
		}
		m.SourceDestination = destName
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp > out[j].Timestamp
	})
	return out, nil
}

// LatestPerTarget walks every target prefix in the destination and
// returns the newest MetaFile for each. Used by the dashboard to render
// a one-row-per-target overview without fetching full histories.
func LatestPerTarget(ctx context.Context, st storage.Storage) (map[string]*MetaFile, error) {
	objs, err := st.List(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list root: %w", err)
	}
	// Group meta files by their target prefix (first path segment).
	byTarget := make(map[string]string)
	for _, o := range objs {
		if !strings.HasSuffix(o.Path, ".meta.json") {
			continue
		}
		parts := strings.SplitN(o.Path, "/", 2)
		if len(parts) < 2 {
			continue
		}
		target := parts[0]
		// Lexical comparison works because timestamps are 20060102T150405Z.
		if existing, ok := byTarget[target]; !ok || o.Path > existing {
			byTarget[target] = o.Path
		}
	}
	out := make(map[string]*MetaFile, len(byTarget))
	for target, p := range byTarget {
		m, err := fetchOne(ctx, st, p)
		if err != nil {
			continue
		}
		out[target] = m
	}
	return out, nil
}

// MedianDuration returns the median run duration over the most recent
// successful runs of target, plus the sample size that produced it.
//
// Only metas with Status==success and DurationSeconds>0 are considered.
// Failed runs tend to be much faster (and are far rarer than successes), so
// including them would systematically underestimate the duration of a real
// run. Legacy metas written before duration was persisted have
// DurationSeconds==0 and are also skipped — sample-size==0 is a meaningful
// signal to the caller (no estimate available).
//
// n caps how many recent runs to consider. Median is preferred over mean
// to stay robust against single-run outliers (e.g. a one-off index rebuild
// that doubled the dump time).
func MedianDuration(ctx context.Context, st storage.Storage, target string, n int) (time.Duration, int, error) {
	all, err := List(ctx, st, target)
	if err != nil {
		return 0, 0, err
	}
	d, count := MedianDurationFromList(all, n)
	return d, count, nil
}

// MedianDurationFromList computes the median over an already-loaded slice of
// metas (assumed sorted newest-first, as List returns them). Useful when the
// caller has already merged metas across destinations and wants to avoid a
// second storage round-trip.
func MedianDurationFromList(all []*MetaFile, n int) (time.Duration, int) {
	if n <= 0 {
		n = 10
	}
	durations := make([]float64, 0, n)
	for _, m := range all {
		if len(durations) >= n {
			break
		}
		if m.IsFailure() || m.DurationSeconds <= 0 {
			continue
		}
		durations = append(durations, m.DurationSeconds)
	}
	if len(durations) == 0 {
		return 0, 0
	}
	sort.Float64s(durations)
	mid := len(durations) / 2
	var median float64
	if len(durations)%2 == 1 {
		median = durations[mid]
	} else {
		median = (durations[mid-1] + durations[mid]) / 2
	}
	return time.Duration(median * float64(time.Second)), len(durations)
}

func fetchOne(ctx context.Context, st storage.Storage, p string) (*MetaFile, error) {
	rc, err := st.Get(ctx, p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	var m MetaFile
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	m.Path = p
	return &m, nil
}
