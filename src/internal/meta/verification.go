package meta

import (
	"fmt"
	"sort"

	"backup-operator/dumper"
)

// BuildVerification compares pre-dump stats, post-dump stats, and
// dump row counts to produce a DumpVerification result.
//
// For MongoDB (or any DB where dump row counting is not feasible),
// dumpCounts will be nil — the verification uses only pre/post comparison.
func BuildVerification(
	preStats, postStats *dumper.Stats,
	dumpCounts map[string]int64,
	dbType string,
) *DumpVerification {
	if preStats == nil {
		return &DumpVerification{
			Verdict: VerificationSkipped,
			Summary: "pre-dump stats not available",
		}
	}

	v := &DumpVerification{
		PreStats:      preStats,
		PostStats:     postStats,
		DumpRowCounts: dumpCounts,
	}

	// Build per-table verification
	allTables := collectTableNames(preStats, postStats, dumpCounts)
	preIndex := indexByName(preStats.Tables)
	postIndex := make(map[string]int64)
	if postStats != nil {
		for _, t := range postStats.Tables {
			postIndex[t.Name] = t.RowCount
		}
	}

	var matchCount, mismatchCount, warnCount int
	hasDumpCounts := len(dumpCounts) > 0

	for _, name := range allTables {
		tv := TableVerification{Name: name}

		pre, hasPre := preIndex[name]
		if hasPre {
			tv.PreDumpRows = pre
		}

		post, hasPost := postIndex[name]
		if hasPost {
			tv.PostDumpRows = post
		}

		dumpRows, hasDump := dumpCounts[name]
		if hasDump {
			tv.DumpRows = dumpRows
		}

		// Determine verdict
		switch {
		case hasDump && hasPre:
			// Compare dump rows to pre-dump rows
			if dumpRows == pre {
				tv.Verdict = VerificationMatch
				matchCount++
			} else if dumpRows >= pre {
				// More rows in dump than pre-dump: concurrent inserts during dump — OK
				tv.Verdict = VerificationMatch
				tv.Detail = fmt.Sprintf("+%d rows during dump (concurrent inserts)", dumpRows-pre)
				matchCount++
			} else {
				// Fewer rows in dump: might indicate concurrent deletes or truncation
				diff := pre - dumpRows
				ratio := float64(dumpRows) / float64(pre)
				if ratio >= 0.99 {
					// Within 1% — close enough for estimated counts
					tv.Verdict = VerificationMatch
					tv.Detail = fmt.Sprintf("-%d rows (within estimation tolerance)", diff)
					matchCount++
				} else {
					tv.Verdict = VerificationMismatch
					tv.Detail = fmt.Sprintf("dump has %d rows vs pre-dump %d (%.1f%%)", dumpRows, pre, ratio*100)
					mismatchCount++
				}
			}
		case !hasDump && hasPre && hasPost:
			// No dump counting (e.g. mongo) — compare pre/post
			if post >= pre {
				tv.Verdict = VerificationMatch
				matchCount++
			} else {
				diff := pre - post
				ratio := float64(post) / float64(pre)
				if ratio >= 0.95 {
					tv.Verdict = VerificationMatch
					tv.Detail = fmt.Sprintf("-%d rows between pre/post (within tolerance)", diff)
					matchCount++
				} else {
					tv.Verdict = VerificationMismatch
					tv.Detail = fmt.Sprintf("post-dump %d vs pre-dump %d (%.1f%%)", post, pre, ratio*100)
					mismatchCount++
				}
			}
		case hasPre && !hasPost && !hasDump:
			tv.Verdict = VerificationSkipped
			tv.Detail = "no post-dump stats or dump row count"
			warnCount++
		default:
			tv.Verdict = VerificationSkipped
			warnCount++
		}

		v.Tables = append(v.Tables, tv)
	}

	// Overall verdict
	totalTables := matchCount + mismatchCount + warnCount
	switch {
	case mismatchCount > 0:
		v.Verdict = VerificationMismatch
		v.Summary = fmt.Sprintf("%d/%d tables verified, %d mismatches detected", matchCount, totalTables, mismatchCount)
	case warnCount > 0 && matchCount > 0:
		v.Verdict = VerificationPartial
		v.Summary = fmt.Sprintf("%d/%d tables verified, %d skipped", matchCount, totalTables, warnCount)
	case matchCount > 0:
		v.Verdict = VerificationMatch
		if hasDumpCounts {
			v.Summary = fmt.Sprintf("all %d tables verified — dump row counts match pre-dump stats", matchCount)
		} else {
			v.Summary = fmt.Sprintf("all %d tables verified — pre/post row counts consistent", matchCount)
		}
	default:
		v.Verdict = VerificationSkipped
		v.Summary = "insufficient data for verification"
	}

	return v
}

func collectTableNames(pre, post *dumper.Stats, dumpCounts map[string]int64) []string {
	seen := make(map[string]bool)
	if pre != nil {
		for _, t := range pre.Tables {
			seen[t.Name] = true
		}
	}
	if post != nil {
		for _, t := range post.Tables {
			seen[t.Name] = true
		}
	}
	for name := range dumpCounts {
		seen[name] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func indexByName(tables []dumper.TableStats) map[string]int64 {
	m := make(map[string]int64, len(tables))
	for _, t := range tables {
		m[t.Name] = t.RowCount
	}
	return m
}
