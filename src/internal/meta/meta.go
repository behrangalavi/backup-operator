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
	Stats              *dumper.Stats    `json:"stats,omitempty"`
	Report             *analyzer.Report `json:"report,omitempty"`
	Destinations       []DestinationResult `json:"destinations,omitempty"`

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
