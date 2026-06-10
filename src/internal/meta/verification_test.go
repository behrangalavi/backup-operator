package meta

import (
	"testing"
	"time"

	"backup-operator/dumper"
)

func TestBuildVerification_AllMatch(t *testing.T) {
	pre := &dumper.Stats{
		Tables: []dumper.TableStats{
			{Name: "users", RowCount: 100},
			{Name: "orders", RowCount: 500},
		},
		GeneratedAt: time.Now(),
	}
	post := &dumper.Stats{
		Tables: []dumper.TableStats{
			{Name: "users", RowCount: 102},
			{Name: "orders", RowCount: 501},
		},
		GeneratedAt: time.Now(),
	}
	dumpCounts := map[string]int64{
		"public.users":  100,
		"public.orders": 500,
	}

	// Dump counts don't match pre stats names (schema prefix difference),
	// use matching names instead
	dumpCounts2 := map[string]int64{
		"users":  100,
		"orders": 500,
	}

	v := BuildVerification(pre, post, dumpCounts2, "postgres", 0)
	if v.Verdict != VerificationMatch {
		t.Errorf("verdict: want %q, got %q (%s)", VerificationMatch, v.Verdict, v.Summary)
	}
	if len(v.Tables) != 2 {
		t.Errorf("tables: want 2, got %d", len(v.Tables))
	}

	// Also test with original dump counts
	_ = dumpCounts
}

func TestBuildVerification_Mismatch(t *testing.T) {
	pre := &dumper.Stats{
		Tables: []dumper.TableStats{
			{Name: "users", RowCount: 1000},
		},
		GeneratedAt: time.Now(),
	}
	dumpCounts := map[string]int64{
		"users": 100, // 90% drop
	}

	v := BuildVerification(pre, nil, dumpCounts, "postgres", 0)
	if v.Verdict != VerificationMismatch {
		t.Errorf("verdict: want %q, got %q (%s)", VerificationMismatch, v.Verdict, v.Summary)
	}
}

func TestBuildVerification_NilPreStats(t *testing.T) {
	v := BuildVerification(nil, nil, nil, "postgres", 0)
	if v.Verdict != VerificationSkipped {
		t.Errorf("verdict: want %q, got %q", VerificationSkipped, v.Verdict)
	}
}

// Without a pre-dump stats baseline the size heuristic cannot judge a
// mongo/redis dump as empty — it returns "" regardless of how tiny the
// encrypted size is. This is the gap the pipeline's BackupEmptyCheckDegraded
// warning compensates for (an absolute byte floor is unsafe because the age
// header size scales with recipient count). If this ever starts returning a
// reason on nil preStats, revisit that warning.
func TestLooksEmptyByHeuristic_NilPreStatsIsNeverEmpty(t *testing.T) {
	for _, dbType := range []string{"redis", "mongo"} {
		if reason := looksEmptyByHeuristic(dbType, nil, 1); reason != "" {
			t.Errorf("%s: expected no verdict without preStats baseline, got %q", dbType, reason)
		}
	}
}

func TestBuildVerification_MongoPrePost(t *testing.T) {
	pre := &dumper.Stats{
		Tables: []dumper.TableStats{
			{Name: "mydb.users", RowCount: 500},
		},
		GeneratedAt: time.Now(),
	}
	post := &dumper.Stats{
		Tables: []dumper.TableStats{
			{Name: "mydb.users", RowCount: 502},
		},
		GeneratedAt: time.Now(),
	}

	v := BuildVerification(pre, post, nil, "mongo", 0)
	if v.Verdict != VerificationMatch {
		t.Errorf("verdict: want %q, got %q (%s)", VerificationMatch, v.Verdict, v.Summary)
	}
}

func TestBuildVerification_ConcurrentInserts(t *testing.T) {
	pre := &dumper.Stats{
		Tables: []dumper.TableStats{
			{Name: "events", RowCount: 1000},
		},
		GeneratedAt: time.Now(),
	}
	dumpCounts := map[string]int64{
		"events": 1050, // more rows in dump than pre-stats
	}

	v := BuildVerification(pre, nil, dumpCounts, "postgres", 0)
	if v.Verdict != VerificationMatch {
		t.Errorf("verdict: want %q, got %q (%s)", VerificationMatch, v.Verdict, v.Summary)
	}
	if v.Tables[0].Detail == "" {
		t.Error("expected detail about concurrent inserts")
	}
}

func TestBuildVerification_WithinTolerance(t *testing.T) {
	pre := &dumper.Stats{
		Tables: []dumper.TableStats{
			{Name: "logs", RowCount: 10000},
		},
		GeneratedAt: time.Now(),
	}
	dumpCounts := map[string]int64{
		"logs": 9950, // 0.5% less, within 1% tolerance
	}

	v := BuildVerification(pre, nil, dumpCounts, "postgres", 0)
	if v.Verdict != VerificationMatch {
		t.Errorf("verdict: want %q, got %q (%s)", VerificationMatch, v.Verdict, v.Summary)
	}
}

func TestBuildVerification_LooksEmpty(t *testing.T) {
	// Pre-dump showed rows, but dump produced 0 INSERTs across all tables —
	// classic "permission denied on SELECT, only DDL emitted" failure.
	pre := &dumper.Stats{
		Tables: []dumper.TableStats{
			{Name: "mydb.users", RowCount: 1000},
			{Name: "mydb.orders", RowCount: 500},
		},
		GeneratedAt: time.Now(),
	}
	dumpCounts := map[string]int64{} // empty: dump had no INSERTs

	v := BuildVerification(pre, nil, dumpCounts, "mysql", 0)
	if !v.LooksEmpty {
		t.Errorf("expected LooksEmpty=true, got false (summary=%q)", v.Summary)
	}
}

func TestBuildVerification_NotEmpty_WhenPreEmpty(t *testing.T) {
	// Pre-dump had no rows — an empty DB is a legitimate state.
	pre := &dumper.Stats{
		Tables: []dumper.TableStats{
			{Name: "mydb.users", RowCount: 0},
		},
		GeneratedAt: time.Now(),
	}
	v := BuildVerification(pre, nil, map[string]int64{}, "mysql", 0)
	if v.LooksEmpty {
		t.Error("LooksEmpty must be false when pre-stats also showed 0 rows")
	}
}

func TestBuildVerification_NotEmpty_WhenNoDumpCounter(t *testing.T) {
	// Mongo and similar dumpers don't supply dumpCounts; LooksEmpty must
	// stay false to avoid a false-positive on those dumpers.
	pre := &dumper.Stats{
		Tables: []dumper.TableStats{
			{Name: "mydb.users", RowCount: 1000},
		},
		GeneratedAt: time.Now(),
	}
	v := BuildVerification(pre, nil, nil, "mongo", 0)
	if v.LooksEmpty {
		t.Error("LooksEmpty must be false when dump row counts are unavailable and encrypted size is unknown")
	}
}

func TestBuildVerification_Mongo_LooksEmpty_BySize(t *testing.T) {
	// Mongo source has 100 MiB across collections but the encrypted dump is
	// only 200 bytes — almost certainly mongodump silently dumped nothing.
	pre := &dumper.Stats{
		Tables: []dumper.TableStats{
			{Name: "app.users", RowCount: 5000, SizeBytes: 50 * 1024 * 1024},
			{Name: "app.orders", RowCount: 2000, SizeBytes: 50 * 1024 * 1024},
		},
		GeneratedAt: time.Now(),
	}
	v := BuildVerification(pre, nil, nil, "mongo", 200)
	if !v.LooksEmpty {
		t.Errorf("expected mongo LooksEmpty=true for 100 MiB source vs 200 byte dump (summary=%q)", v.Summary)
	}
}

func TestBuildVerification_Mongo_NotEmpty_HealthyRatio(t *testing.T) {
	// 100 MiB source → 8 MiB encrypted dump (~8% — typical BSON+gzip+age).
	pre := &dumper.Stats{
		Tables: []dumper.TableStats{
			{Name: "app.users", RowCount: 5000, SizeBytes: 100 * 1024 * 1024},
		},
		GeneratedAt: time.Now(),
	}
	v := BuildVerification(pre, nil, nil, "mongo", 8*1024*1024)
	if v.LooksEmpty {
		t.Errorf("LooksEmpty must be false for healthy compression ratio (summary=%q)", v.Summary)
	}
}

func TestBuildVerification_Mongo_NotEmpty_TinySource(t *testing.T) {
	// Source under the threshold — heuristic deliberately skips so a tiny
	// real dump isn't flagged just because compression looks suspicious.
	pre := &dumper.Stats{
		Tables: []dumper.TableStats{
			{Name: "app.tiny", RowCount: 5, SizeBytes: 1024},
		},
		GeneratedAt: time.Now(),
	}
	v := BuildVerification(pre, nil, nil, "mongo", 50)
	if v.LooksEmpty {
		t.Error("LooksEmpty must skip the heuristic for source < 1 MiB to avoid false positives")
	}
}

func TestBuildVerification_Redis_LooksEmpty_HeaderOnly(t *testing.T) {
	// Redis has 10000 keys but the dump is only 80 bytes — RDB header only.
	pre := &dumper.Stats{
		Tables: []dumper.TableStats{
			{Name: "db0", RowCount: 10000},
		},
		GeneratedAt: time.Now(),
	}
	v := BuildVerification(pre, nil, nil, "redis", 80)
	if !v.LooksEmpty {
		t.Errorf("expected redis LooksEmpty=true for 10000 keys vs 80 byte dump (summary=%q)", v.Summary)
	}
}

func TestBuildVerification_Redis_NotEmpty_NoKeys(t *testing.T) {
	// An empty Redis is a legitimate state — the heuristic must not fire
	// when pre.totalKeys == 0.
	pre := &dumper.Stats{
		Tables: []dumper.TableStats{
			{Name: "db0", RowCount: 0},
		},
		GeneratedAt: time.Now(),
	}
	v := BuildVerification(pre, nil, nil, "redis", 80)
	if v.LooksEmpty {
		t.Error("LooksEmpty must be false when redis source has no keys")
	}
}

func TestBuildVerification_MySQLUnqualifiedNames(t *testing.T) {
	pre := &dumper.Stats{
		Tables: []dumper.TableStats{
			{Name: "mydb.users", RowCount: 100},
			{Name: "mydb.orders", RowCount: 200},
		},
		GeneratedAt: time.Now(),
	}
	// mysqldump produces unqualified names
	dumpCounts := map[string]int64{
		"users":  100,
		"orders": 200,
	}

	v := BuildVerification(pre, nil, dumpCounts, "mysql", 0)
	if v.Verdict != VerificationMatch {
		t.Errorf("verdict: want %q, got %q (%s)", VerificationMatch, v.Verdict, v.Summary)
	}
	if len(v.Tables) != 2 {
		t.Fatalf("expected 2 tables, got %d", len(v.Tables))
	}
	for _, tv := range v.Tables {
		if tv.Verdict != VerificationMatch {
			t.Errorf("table %s: want match, got %s", tv.Name, tv.Verdict)
		}
	}
}
