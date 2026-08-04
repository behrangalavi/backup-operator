package backup

import (
	"crypto/sha256"
	"encoding/hex"

	"backup-operator/analyzer"
	"backup-operator/dumper"
	"backup-operator/internal/meta"
	"backup-operator/metrics"
)

func emitAnalyzerMetrics(target string, r *analyzer.Report, realStats *dumper.Stats) {
	if r == nil {
		return
	}
	if r.SizeChangeRatio > 0 {
		metrics.SetDumpSizeChangeRatio(target, r.SizeChangeRatio)
	}
	metrics.SetSchemaChanged(target, r.SchemaChanged)
	metrics.SetCharsetChanged(target, r.CharsetChanged)
	// Use realStats for table-row-count labels even when the source is
	// anonymizing for storage. ADR §18: Prometheus stays scrape-only and
	// keeps real table names; only meta.json gets hashed names.
	if realStats != nil {
		metrics.SetTableCount(target, len(realStats.Tables))
		for _, t := range realStats.Tables {
			metrics.SetTableRowCount(target, t.Name, t.RowCount)
		}
	}
	metrics.SetLastRunAnomalies(target, len(r.Anomalies))
}

func hashTableName(name string) string {
	h := sha256.Sum256([]byte(name))
	return hex.EncodeToString(h[:8])
}

// looksAnonymized reports whether every table name in s is already a 16-hex
// hash (the format hashTableName produces). Used to detect the transition
// run when anonymize-tables was just toggled on: the last meta.json was
// written with real names, so prev stats need a one-shot hash before they
// can line up with the current run's hashed names.
func looksAnonymized(s *dumper.Stats) bool {
	if s == nil || len(s.Tables) == 0 {
		return true
	}
	for _, t := range s.Tables {
		if len(t.Name) != 16 {
			return false
		}
		for i := 0; i < 16; i++ {
			c := t.Name[i]
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
			if !isHex {
				return false
			}
		}
	}
	return true
}

func anonymizeStats(s *dumper.Stats) *dumper.Stats {
	// Charset/Collation are database-level metadata, not table names, so they
	// carry through anonymisation unchanged. Dropping them here (as this used
	// to) left cmpStats with empty charset fields, so the analyzer's
	// charset-drift comparison saw "no current charset" on every anonymised
	// run — BackupCharsetChanged could never fire and the persisted baseline
	// lost the fields too, blinding all future runs.
	anon := &dumper.Stats{
		SchemaHash:  s.SchemaHash,
		Charset:     s.Charset,
		Collation:   s.Collation,
		GeneratedAt: s.GeneratedAt,
		Tables:      make([]dumper.TableStats, len(s.Tables)),
	}
	for i, t := range s.Tables {
		anon.Tables[i] = dumper.TableStats{
			Name:      hashTableName(t.Name),
			RowCount:  t.RowCount,
			SizeBytes: t.SizeBytes,
		}
	}
	return anon
}

func anonymizeVerification(v *meta.DumpVerification) *meta.DumpVerification {
	anon := &meta.DumpVerification{
		Verdict: v.Verdict,
		Summary: v.Summary,
	}
	if v.PreStats != nil {
		anon.PreStats = anonymizeStats(v.PreStats)
	}
	if v.PostStats != nil {
		anon.PostStats = anonymizeStats(v.PostStats)
	}
	if len(v.DumpRowCounts) > 0 {
		anon.DumpRowCounts = make(map[string]int64, len(v.DumpRowCounts))
		for k, c := range v.DumpRowCounts {
			anon.DumpRowCounts[hashTableName(k)] = c
		}
	}
	if len(v.Tables) > 0 {
		anon.Tables = make([]meta.TableVerification, len(v.Tables))
		for i, t := range v.Tables {
			anon.Tables[i] = meta.TableVerification{
				Name:         hashTableName(t.Name),
				PreDumpRows:  t.PreDumpRows,
				PostDumpRows: t.PostDumpRows,
				DumpRows:     t.DumpRows,
				Verdict:      t.Verdict,
				Detail:       t.Detail,
			}
		}
	}
	return anon
}
