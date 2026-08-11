package meta

import (
	"fmt"
	"sort"
	"strings"

	"backup-operator/dumper"
)

// BuildVerification compares pre-dump stats, post-dump stats, and
// dump row counts to produce a DumpVerification result.
//
// For MongoDB (or any DB where dump row counting is not feasible),
// dumpCounts will be nil — the verification uses only pre/post comparison.
//
// encryptedSize is the size of the produced ciphertext on disk; it powers
// engine-specific empty-dump heuristics for backends where row counting is
// not available (mongo, redis). Pass 0 if not yet known — the heuristic is
// then skipped.
func BuildVerification(
	preStats, postStats *dumper.Stats,
	dumpCounts map[string]int64,
	dbType string,
	encryptedSize int64,
	rawDumpSize int64,
) *DumpVerification {
	if preStats == nil {
		return &DumpVerification{
			Verdict: VerificationSkipped,
			Summary: "pre-dump stats not available",
		}
	}

	// Normalize dump counts: mysqldump produces unqualified names (e.g.
	// "users") while CollectStats uses schema-qualified names ("mydb.users").
	// Map unqualified dump names to their schema-qualified equivalents.
	normalizedDumpCounts := normalizeDumpCounts(dumpCounts, preStats, postStats)

	v := &DumpVerification{
		PreStats:      preStats,
		PostStats:     postStats,
		DumpRowCounts: normalizedDumpCounts,
	}

	// Build per-table verification
	allTables := collectTableNames(preStats, postStats, normalizedDumpCounts)
	preIndex := indexByName(preStats.Tables)
	postIndex := make(map[string]int64)
	if postStats != nil {
		for _, t := range postStats.Tables {
			postIndex[t.Name] = t.RowCount
		}
	}

	var matchCount, mismatchCount, warnCount int
	hasDumpCounts := len(normalizedDumpCounts) > 0

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

		dumpRows, hasDump := normalizedDumpCounts[name]
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

	// "Looks empty" detection has two paths:
	//   - SQL (postgres/mysql/mariadb): the row counter saw the dump stream.
	//     dumpCounts is non-nil; an empty map means the dump produced 0
	//     INSERTs despite pre-stats showing rows.
	//   - Non-SQL (mongo/redis): no row counter. We use encryptedSize as a
	//     proxy — a healthy dump compresses to ~10-30% of source size, so a
	//     dump that's <1% of the pre-dump source size with significant data
	//     in the source is almost certainly broken.
	if dumpCounts != nil {
		var preRowTotal, dumpRowTotal int64
		for _, t := range preStats.Tables {
			preRowTotal += t.RowCount
		}
		for _, c := range normalizedDumpCounts {
			dumpRowTotal += c
		}
		if preRowTotal > 0 && dumpRowTotal == 0 {
			v.LooksEmpty = true
			v.Summary = fmt.Sprintf(
				"empty dump: pre-dump showed %d rows but dump contains 0 INSERTs (likely permission issue)",
				preRowTotal,
			)
		}
	} else if encryptedSize > 0 {
		if reason := looksEmptyByHeuristic(dbType, preStats, encryptedSize, rawDumpSize); reason != "" {
			v.LooksEmpty = true
			v.Summary = reason
		}
	}

	return v
}

// looksEmptyByHeuristic detects empty dumps for engines without a stream-level
// row counter. Returns "" when the dump looks plausibly populated; returns a
// human-readable reason otherwise.
//
// Mongo: BSON+gzip typically compresses to ~10-30% of source. A dump that's
// less than 1% of preStats total size with > 1 MiB of source data is almost
// certainly broken. Threshold tuned conservatively to avoid false-positives
// on highly compressible / mostly-empty databases.
//
// Redis: RDB has a fixed 50-byte header + per-key serialisation. With any
// keys present, the encrypted dump should clear ~200 bytes easily; anything
// smaller means redis-cli emitted only the header.
func looksEmptyByHeuristic(dbType string, preStats *dumper.Stats, encryptedSize, rawDumpSize int64) string {
	if preStats == nil {
		return ""
	}
	switch dbType {
	case "mongo":
		var preTotalSize int64
		for _, t := range preStats.Tables {
			preTotalSize += t.SizeBytes
		}
		const minSourceMiB = 1 << 20
		const ratioThreshold = 0.01
		if preTotalSize > minSourceMiB && float64(encryptedSize) < float64(preTotalSize)*ratioThreshold {
			return fmt.Sprintf(
				"empty dump heuristic: source has %d MiB across collections but encrypted dump is only %d bytes (<%.0f%% expected)",
				preTotalSize>>20, encryptedSize, ratioThreshold*100,
			)
		}
	case "redis":
		var preTotalKeys int64
		for _, t := range preStats.Tables {
			preTotalKeys += t.RowCount
		}
		// Check the RAW (pre-compression, pre-encryption) RDB size, not the
		// encrypted size: age+gzip overhead alone is a few hundred bytes, so
		// the old `encryptedSize < 200` could never fire — a header-only RDB
		// (source has keys, dump is just the "REDIS00xx" preamble) was reported
		// as a successful backup. A real RDB carrying even one key is far larger
		// than a bare header; 200 raw bytes is comfortably below any real
		// keyset yet above the ~9-40 byte header.
		const minRawBytesWithKeys = 200
		if preTotalKeys > 0 && rawDumpSize < minRawBytesWithKeys {
			return fmt.Sprintf(
				"empty dump heuristic: source has %d keys but raw RDB is only %d bytes (header-only)",
				preTotalKeys, rawDumpSize,
			)
		}
	}
	return ""
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

// normalizeDumpCounts resolves name mismatches between dump-parsed table
// names and stats-collected table names. mysqldump emits unqualified names
// ("users") while CollectStats uses "schema.table" ("mydb.users"). For
// each unqualified dump name, if a stats table ends with "."+name, the
// count is remapped to the qualified name.
func normalizeDumpCounts(dumpCounts map[string]int64, preStats, postStats *dumper.Stats) map[string]int64 {
	if len(dumpCounts) == 0 {
		return dumpCounts
	}

	// Build a lookup of all known qualified names from stats.
	qualified := make(map[string]string) // unqualified → qualified
	addQualified := func(tables []dumper.TableStats) {
		for _, t := range tables {
			idx := strings.LastIndexByte(t.Name, '.')
			if idx > 0 {
				short := t.Name[idx+1:]
				qualified[short] = t.Name
			}
		}
	}
	if preStats != nil {
		addQualified(preStats.Tables)
	}
	if postStats != nil {
		addQualified(postStats.Tables)
	}

	if len(qualified) == 0 {
		return dumpCounts
	}

	out := make(map[string]int64, len(dumpCounts))
	for name, count := range dumpCounts {
		if strings.ContainsRune(name, '.') {
			// Already qualified
			out[name] = count
			continue
		}
		if q, ok := qualified[name]; ok {
			out[q] += count
		} else {
			out[name] = count
		}
	}
	return out
}
