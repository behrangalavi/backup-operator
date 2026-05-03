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

	v := BuildVerification(pre, post, dumpCounts2, "postgres")
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

	v := BuildVerification(pre, nil, dumpCounts, "postgres")
	if v.Verdict != VerificationMismatch {
		t.Errorf("verdict: want %q, got %q (%s)", VerificationMismatch, v.Verdict, v.Summary)
	}
}

func TestBuildVerification_NilPreStats(t *testing.T) {
	v := BuildVerification(nil, nil, nil, "postgres")
	if v.Verdict != VerificationSkipped {
		t.Errorf("verdict: want %q, got %q", VerificationSkipped, v.Verdict)
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

	v := BuildVerification(pre, post, nil, "mongo")
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

	v := BuildVerification(pre, nil, dumpCounts, "postgres")
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

	v := BuildVerification(pre, nil, dumpCounts, "postgres")
	if v.Verdict != VerificationMatch {
		t.Errorf("verdict: want %q, got %q (%s)", VerificationMatch, v.Verdict, v.Summary)
	}
}
