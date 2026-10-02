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
		tv, class := verifyTable(name, preIndex, postIndex, normalizedDumpCounts)
		switch class {
		case verificationMatchClass:
			matchCount++
		case verificationMismatchClass:
			mismatchCount++
		default:
			warnCount++
		}
		v.Tables = append(v.Tables, tv)
	}

	v.Verdict, v.Summary = overallVerdict(matchCount, mismatchCount, warnCount, hasDumpCounts)

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
// Redis: a header-only RDB (what redis-cli --rdb emits when the server
// returns no keys) is ~40–100 bytes depending on the aux fields the server
// version writes. The floor scales with the key count — see
// redisMinRawBytes — so small legitimate keysets are never flagged.
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
		// encrypted size: age+gzip overhead alone is a few hundred bytes, so a
		// check on encryptedSize could never fire for a header-only RDB.
		if floor := redisMinRawBytes(preTotalKeys); preTotalKeys > 0 && rawDumpSize < floor {
			return fmt.Sprintf(
				"empty dump heuristic: source has %d keys but raw RDB is only %d bytes (below the %d-byte floor a %d-key RDB must exceed; header-only)",
				preTotalKeys, rawDumpSize, floor, preTotalKeys,
			)
		}
	}
	return ""
}

// RDB size floor constants for redisMinRawBytes.
//
// rdbMinimalHeaderBytes is the smallest RDB redis can emit at all: the
// "REDIS0011" magic (9 bytes), the EOF opcode (1) and the CRC64 trailer (8).
// Real servers add aux fields (redis-ver, redis-bits, ctime, used-mem,
// aof-base) and land at ~80–100 bytes, but the floor must never exceed what a
// legitimate dump can be, so the bare minimum is used.
//
// rdbMinBytesPerKey is the smallest on-disk footprint of one key: a value-type
// opcode (1), a length-prefixed key (1-byte length + at least 1 byte) and a
// length-prefixed value (at least the 1-byte length of an empty string). Four
// bytes is therefore physically impossible to undercut — a real key with a
// realistic name and value is 15–40 bytes — which is what makes the floor a
// safe lower bound rather than a tuned threshold.
const (
	rdbMinimalHeaderBytes = 18
	rdbMinBytesPerKey     = 4
)

// redisMinRawBytes is the size a raw RDB holding `keys` keys cannot be below.
// A dump smaller than this with keys > 0 is a header-only artifact. The
// previous flat 200-byte floor false-positived legitimate 5–10-key keysets
// (~120–160 raw bytes); the trade-off accepted now is that for very small
// keysets (roughly < 20 keys) a header-only dump can slip through, because
// its ~40–100 bytes may exceed 18 + 4·keys — the project prefers a rare miss
// over a false hard-fail on a healthy backup.
func redisMinRawBytes(keys int64) int64 {
	return rdbMinimalHeaderBytes + rdbMinBytesPerKey*keys
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

// verdictClass buckets a per-table verdict for the overall counters.
type verdictClass int

const (
	verificationWarnClass verdictClass = iota
	verificationMatchClass
	verificationMismatchClass
)

// verifyTable derives one table's verdict from whichever counts exist:
// dump-stream rows vs pre-dump stats (SQL engines), or pre/post stats when
// no stream count exists (mongo/redis). Tolerances: 1 % for stream counts
// (pre-stats are estimates), 5 % for pre/post drift during the dump.
func verifyTable(name string, preIndex, postIndex map[string]int64, dumpCounts map[string]int64) (TableVerification, verdictClass) {
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

	switch {
	case hasDump && hasPre:
		return compareRows(tv, dumpRows, pre, 0.99, "dump has %d rows vs pre-dump %d (%.1f%%)", "-%d rows (within estimation tolerance)", "+%d rows during dump (concurrent inserts)")
	case !hasDump && hasPre && hasPost:
		return compareRows(tv, post, pre, 0.95, "post-dump %d vs pre-dump %d (%.1f%%)", "-%d rows between pre/post (within tolerance)", "")
	case hasPre && !hasPost && !hasDump:
		tv.Verdict = VerificationSkipped
		tv.Detail = "no post-dump stats or dump row count"
		return tv, verificationWarnClass
	default:
		tv.Verdict = VerificationSkipped
		return tv, verificationWarnClass
	}
}

// compareRows compares an observed row count against the pre-dump count.
// More rows than pre is always a match (concurrent inserts); fewer rows
// within the tolerance ratio is a match with a note; below it a mismatch.
func compareRows(tv TableVerification, got, pre int64, tolerance float64, mismatchFmt, withinFmt, moreFmt string) (TableVerification, verdictClass) {
	switch {
	case got == pre:
		tv.Verdict = VerificationMatch
	case got > pre:
		tv.Verdict = VerificationMatch
		if moreFmt != "" {
			tv.Detail = fmt.Sprintf(moreFmt, got-pre)
		}
	default:
		diff := pre - got
		ratio := float64(got) / float64(pre)
		if ratio >= tolerance {
			tv.Verdict = VerificationMatch
			tv.Detail = fmt.Sprintf(withinFmt, diff)
		} else {
			tv.Verdict = VerificationMismatch
			tv.Detail = fmt.Sprintf(mismatchFmt, got, pre, ratio*100)
			return tv, verificationMismatchClass
		}
	}
	return tv, verificationMatchClass
}

// overallVerdict folds the per-table counters into the run verdict.
func overallVerdict(matchCount, mismatchCount, warnCount int, hasDumpCounts bool) (string, string) {
	totalTables := matchCount + mismatchCount + warnCount
	switch {
	case mismatchCount > 0:
		return VerificationMismatch, fmt.Sprintf("%d/%d tables verified, %d mismatches detected", matchCount, totalTables, mismatchCount)
	case warnCount > 0 && matchCount > 0:
		return VerificationPartial, fmt.Sprintf("%d/%d tables verified, %d skipped", matchCount, totalTables, warnCount)
	case matchCount > 0:
		if hasDumpCounts {
			return VerificationMatch, fmt.Sprintf("all %d tables verified — dump row counts match pre-dump stats", matchCount)
		}
		return VerificationMatch, fmt.Sprintf("all %d tables verified — pre/post row counts consistent", matchCount)
	default:
		return VerificationSkipped, "insufficient data for verification"
	}
}
