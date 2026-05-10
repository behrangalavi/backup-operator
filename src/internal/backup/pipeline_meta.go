package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"time"

	"backup-operator/analyzer"
	"backup-operator/dumper"
	"backup-operator/internal/meta"
	"backup-operator/internal/secrets"
	"backup-operator/metrics"
	"backup-operator/storage"
	storageFactory "backup-operator/storage/factory"
)

// loadPreviousMeta returns the most recent successful meta across destinations.
// Failure-metas are skipped so a transient failure does not blank the analyzer's
// baseline. Returns nil when no successful run is yet stored anywhere.
//
// As a side effect, sets backup_operator_analyzer_baseline_unavailable: 1 when
// every destination failed before producing a readable response (every storage
// init / list errored), 0 otherwise — including the legitimate first-run case
// where destinations responded but no successful meta exists yet. This lets
// alerting distinguish "this target has never run" (silent) from "every
// destination is broken so the analyzer is running blind" (loud).
func (p *Pipeline) loadPreviousMeta(ctx context.Context, dests []*secrets.Destination, target string) *meta.MetaFile {
	// destAccessed = at least one destination's storage init AND List
	// succeeded. If all destinations fail before that point, the analyzer
	// is running blind and the operator should know.
	destAccessed := 0
	for _, d := range dests {
		st, err := storageFactory.NewStorage(d.StorageType, d.Name, d.Data, p.logger)
		if err != nil {
			p.logger.V(1).Info("baseline: storage init failed", "target", target, "destination", d.Name, "err", err.Error())
			continue
		}
		objs, err := st.List(ctx, target+"/")
		if err != nil {
			p.logger.V(1).Info("baseline: list failed", "target", target, "destination", d.Name, "err", err.Error())
			continue
		}
		destAccessed++
		if len(objs) == 0 {
			continue
		}
		for _, op := range sortedMetaPaths(objs) {
			rc, err := st.Get(ctx, op)
			if err != nil {
				p.logger.V(1).Info("baseline: get failed", "target", target, "destination", d.Name, "path", op, "err", err.Error())
				continue
			}
			raw, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				continue
			}
			var m meta.MetaFile
			if err := json.Unmarshal(raw, &m); err != nil {
				continue
			}
			if m.IsFailure() {
				continue
			}
			metrics.SetAnalyzerBaselineUnavailable(target, false)
			return &m
		}
	}
	// No successful baseline. Distinguish "every destination broken" from
	// "no baseline yet" — both return nil to the caller, the gauge tells
	// monitoring which case it is.
	if len(dests) > 0 && destAccessed == 0 {
		p.logger.Info("analyzer baseline unavailable; every destination failed",
			"target", target, "destinations", len(dests))
		metrics.SetAnalyzerBaselineUnavailable(target, true)
	} else {
		metrics.SetAnalyzerBaselineUnavailable(target, false)
	}
	return nil
}

// sortedMetaPaths returns meta paths newest-first by LastModified.
func sortedMetaPaths(objs []storage.Object) []string {
	metas := make([]storage.Object, 0, len(objs))
	for _, o := range objs {
		if path.Ext(o.Path) != ".json" {
			continue
		}
		metas = append(metas, o)
	}
	sort.Slice(metas, func(i, j int) bool {
		return metas[i].LastModified.After(metas[j].LastModified)
	})
	out := make([]string, len(metas))
	for i, m := range metas {
		out[i] = m.Path
	}
	return out
}

func metaJSON(src *secrets.Source, stats *dumper.Stats, statsError string, report *analyzer.Report, verification *meta.DumpVerification, size int64, sha256sum, timestamp string, runStart time.Time, schemaChangedAt time.Time, destResults []meta.DestinationResult, restoreVerification *meta.RestoreVerificationResult, retention []meta.RetentionResult) ([]byte, error) {
	completedAt := time.Now().UTC()
	m := meta.MetaFile{
		Target:              src.TargetName,
		Timestamp:           timestamp,
		DBType:              src.DBType,
		Status:              meta.StatusSuccess,
		EncryptedSizeBytes:  size,
		SHA256:              sha256sum,
		SchemaChangedAt:     schemaChangedAt,
		CompletedAt:         completedAt,
		DurationSeconds:     completedAt.Sub(runStart).Seconds(),
		Stats:               stats,
		StatsError:          statsError,
		Report:              report,
		Verification:        verification,
		RestoreVerification: restoreVerification,
		Destinations:        destResults,
		Retention:           retention,
	}
	return json.MarshalIndent(m, "", "  ")
}

// failureMetaJSON produces the sidecar written when a run never reaches the
// fan-out — there is no dump and no stats, only the cause and the phase
// where it broke.
func failureMetaJSON(src *secrets.Source, timestamp, phase string, runStart time.Time, runErr error) ([]byte, error) {
	msg := ""
	if runErr != nil {
		msg = runErr.Error()
	}
	completedAt := time.Now().UTC()
	m := meta.MetaFile{
		Target:          src.TargetName,
		Timestamp:       timestamp,
		DBType:          src.DBType,
		Status:          meta.StatusFailed,
		Error:           msg,
		Phase:           phase,
		CompletedAt:     completedAt,
		DurationSeconds: completedAt.Sub(runStart).Seconds(),
	}
	return json.MarshalIndent(m, "", "  ")
}

// fallbackMetaJSON hand-builds a minimal valid meta.json for the unlikely
// case json.MarshalIndent of the full struct fails. Without this, a marshal
// error would leave the run with no sidecar at all and the UI would never
// see it. The fields here are the absolute minimum the UI/refresher needs
// to render "this run happened and it broke".
func fallbackMetaJSON(target, timestamp, dbType, phase string, marshalErr error) []byte {
	body, err := json.MarshalIndent(meta.MetaFile{
		Target:    target,
		Timestamp: timestamp,
		DBType:    dbType,
		Status:    meta.StatusFailed,
		Phase:     phase,
		Error:     "meta marshal failed: " + marshalErr.Error(),
	}, "", "  ")
	if err != nil {
		// MarshalIndent of strings cannot fail in practice. If it somehow
		// does, return a syntactically-valid placeholder rather than
		// nothing — empty bytes would be uploaded as a 0-byte object and
		// confuse every consumer.
		return []byte(`{"status":"failed","error":"meta marshal catastrophic failure"}`)
	}
	return body
}

func buildObjectPath(target, timestamp, ext string) string {
	t, err := time.Parse("20060102T150405Z", timestamp)
	if err != nil {
		return path.Join(target, fmt.Sprintf("dump-%s.%s", timestamp, ext))
	}
	return path.Join(
		target,
		fmt.Sprintf("%04d", t.Year()),
		fmt.Sprintf("%02d", t.Month()),
		fmt.Sprintf("%02d", t.Day()),
		fmt.Sprintf("dump-%s.%s", timestamp, ext),
	)
}
