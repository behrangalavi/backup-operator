package backup

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"backup-operator/internal/meta"
	"backup-operator/internal/secrets"
	"backup-operator/storage"

	"github.com/go-logr/logr"
)

// fakeNow is the wall clock used in every test below.
var fakeNow = time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)

// dump produces a (dump, meta) pair as the pipeline would write them.
func dump(target string, t time.Time) []storage.Object {
	ts := t.Format(timestampLayout)
	return []storage.Object{
		{Path: target + "/" + ts[:4] + "/" + ts[4:6] + "/" + ts[6:8] + "/dump-" + ts + ".sql.gz.age", Size: 100},
		{Path: target + "/" + ts[:4] + "/" + ts[4:6] + "/" + ts[6:8] + "/dump-" + ts + ".meta.json", Size: 1},
	}
}

func TestSelectForDeletion_DisabledByDays(t *testing.T) {
	objs := dump("x", fakeNow.AddDate(0, 0, -100))
	got := selectForDeletion(objs, RetentionPolicy{Days: 0, MinKeep: 0}, fakeNow)
	if len(got) != 0 {
		t.Errorf("Days=0 must keep everything, got %v", got)
	}
}

func TestSelectForDeletion_KeepsRecentEvenWhenOld(t *testing.T) {
	// All 4 dumps are older than retention, but min-keep=3 must save 3.
	var objs []storage.Object
	for i := 0; i < 4; i++ {
		objs = append(objs, dump("x", fakeNow.AddDate(0, 0, -100-i))...)
	}

	got := selectForDeletion(objs, RetentionPolicy{Days: 30, MinKeep: 3}, fakeNow)

	// Out of 4 timestamps, 3 are floor-protected, only the oldest pair must go.
	if len(got) != 2 {
		t.Fatalf("expected 2 victims (one dump+meta pair), got %d: %v", len(got), got)
	}
}

func TestSelectForDeletion_DeletesOldKeepsRecent(t *testing.T) {
	// 5 dumps; ages 1d, 5d, 10d, 60d, 90d. Days=30, MinKeep=2.
	// Floor protects the 2 newest. Of the remaining 3, the 60d and 90d are
	// past cutoff (delete = 4 paths); the 10d is within window (keep).
	var objs []storage.Object
	for _, age := range []int{1, 5, 10, 60, 90} {
		objs = append(objs, dump("x", fakeNow.AddDate(0, 0, -age))...)
	}

	got := selectForDeletion(objs, RetentionPolicy{Days: 30, MinKeep: 2}, fakeNow)

	if len(got) != 4 {
		t.Fatalf("expected 4 victims (2 timestamps × dump+meta), got %d: %v", len(got), got)
	}
	// None of the kept timestamps should appear in victims.
	keepStamps := []string{
		fakeNow.AddDate(0, 0, -1).Format(timestampLayout),
		fakeNow.AddDate(0, 0, -5).Format(timestampLayout),
		fakeNow.AddDate(0, 0, -10).Format(timestampLayout),
	}
	for _, v := range got {
		for _, k := range keepStamps {
			if contains(v, k) {
				t.Errorf("victim %s contains kept timestamp %s", v, k)
			}
		}
	}
}

func TestSelectForDeletion_NothingOldEnough(t *testing.T) {
	// All within retention window — nothing to delete.
	var objs []storage.Object
	for _, age := range []int{1, 5, 10, 20} {
		objs = append(objs, dump("x", fakeNow.AddDate(0, 0, -age))...)
	}

	got := selectForDeletion(objs, RetentionPolicy{Days: 30, MinKeep: 1}, fakeNow)

	if len(got) != 0 {
		t.Errorf("nothing old enough, expected 0 victims, got %v", got)
	}
}

func TestSelectForDeletion_DropsBothDumpAndMeta(t *testing.T) {
	objs := dump("x", fakeNow.AddDate(0, 0, -100))
	got := selectForDeletion(objs, RetentionPolicy{Days: 30, MinKeep: 0}, fakeNow)

	sort.Strings(got)
	if len(got) != 2 {
		t.Fatalf("expected dump+meta both deleted, got %v", got)
	}
}

func TestSelectForDeletion_IgnoresUnrelatedFiles(t *testing.T) {
	objs := []storage.Object{
		{Path: "x/2026/04/29/random-file.txt", Size: 1},
		{Path: "x/some-other-thing", Size: 1},
	}
	got := selectForDeletion(objs, RetentionPolicy{Days: 1, MinKeep: 0}, fakeNow)
	if len(got) != 0 {
		t.Errorf("unrelated files must never be touched, got %v", got)
	}
}

func TestSelectForDeletion_MalformedTimestampSurvives(t *testing.T) {
	// A file shaped like a dump but with a garbage timestamp must not be
	// deleted — better to leak storage than delete data of unknown age.
	objs := []storage.Object{
		{Path: "x/dump-not-a-date.sql.gz.age", Size: 1},
	}
	got := selectForDeletion(objs, RetentionPolicy{Days: 1, MinKeep: 0}, fakeNow)
	if len(got) != 0 {
		t.Errorf("malformed timestamp must survive, got %v", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestSelectForDeletion_EmptyInput(t *testing.T) {
	if got := selectForDeletion(nil, RetentionPolicy{Days: 30, MinKeep: 0}, fakeNow); len(got) != 0 {
		t.Errorf("nil input must produce no victims, got %v", got)
	}
	if got := selectForDeletion([]storage.Object{}, RetentionPolicy{Days: 30, MinKeep: 0}, fakeNow); len(got) != 0 {
		t.Errorf("empty input must produce no victims, got %v", got)
	}
}

func TestSelectForDeletion_MinKeepGreaterThanTotal(t *testing.T) {
	// 2 dumps, both ancient, but MinKeep=5 — the floor protects every
	// existing timestamp regardless of age.
	var objs []storage.Object
	for _, age := range []int{100, 200} {
		objs = append(objs, dump("x", fakeNow.AddDate(0, 0, -age))...)
	}
	got := selectForDeletion(objs, RetentionPolicy{Days: 30, MinKeep: 5}, fakeNow)
	if len(got) != 0 {
		t.Errorf("MinKeep > total dumps must keep everything, got %v", got)
	}
}

func TestSelectForDeletion_CutoffBoundary(t *testing.T) {
	// Three dumps at the boundary:
	//   - exactly at cutoff (now - 30d)         → kept (Before == false)
	//   - one nanosecond past cutoff            → deleted
	//   - one nanosecond before cutoff (newer)  → kept
	// Documents the exact semantics so a future tweak to `Before` vs.
	// `!After` can't silently shift retention by one boundary point.
	cutoff := fakeNow.Add(-30 * 24 * time.Hour)
	objs := append(dump("x", cutoff),
		append(dump("x", cutoff.Add(-time.Nanosecond)),
			dump("x", cutoff.Add(time.Nanosecond))...,
		)...,
	)
	got := selectForDeletion(objs, RetentionPolicy{Days: 30, MinKeep: 0}, fakeNow)
	if len(got) != 2 {
		t.Fatalf("only the past-cutoff timestamp should be deleted, got %d victims: %v", len(got), got)
	}
	pastTS := cutoff.Add(-time.Nanosecond).Format(timestampLayout)
	for _, v := range got {
		if !contains(v, pastTS) {
			t.Errorf("expected only the past-cutoff timestamp in victims, got %s", v)
		}
	}
}

func TestSelectForDeletion_OrphanMeta(t *testing.T) {
	// A meta file without a matching dump (or vice versa) should still be
	// pruned when its timestamp is past cutoff. Otherwise stale metas
	// accumulate after a botched delete on a previous run.
	old := fakeNow.AddDate(0, 0, -100).Format(timestampLayout)
	objs := []storage.Object{
		{Path: "x/2026/01/19/dump-" + old + ".meta.json", Size: 1},
	}
	got := selectForDeletion(objs, RetentionPolicy{Days: 30, MinKeep: 0}, fakeNow)
	if len(got) != 1 || !contains(got[0], ".meta.json") {
		t.Errorf("orphan meta must be selected for deletion, got %v", got)
	}
}

// failureMeta produces the sidecar-only artifact a failed run leaves behind:
// a dump-<ts>.meta.json with no dump file.
func failureMeta(target string, t time.Time) []storage.Object {
	ts := t.Format(timestampLayout)
	return []storage.Object{
		{Path: target + "/" + ts[:4] + "/" + ts[4:6] + "/" + ts[6:8] + "/dump-" + ts + ".meta.json", Size: 1},
	}
}

// TestSelectForDeletion_FailureMetasDoNotConsumeFloor is the regression guard
// for the MinKeep bug: a streak of failed runs at the top of the list must not
// push the real dumps below the safety floor and get them age-deleted.
func TestSelectForDeletion_FailureMetasDoNotConsumeFloor(t *testing.T) {
	var objs []storage.Object
	// The 3 newest timestamps are failure-meta-only (DB was down), and recent.
	for i := 0; i < 3; i++ {
		objs = append(objs, failureMeta("x", fakeNow.AddDate(0, 0, -i))...)
	}
	// The only 3 real dumps are all older than the 7-day retention window.
	for _, age := range []int{20, 30, 40} {
		objs = append(objs, dump("x", fakeNow.AddDate(0, 0, -age))...)
	}

	got := selectForDeletion(objs, RetentionPolicy{Days: 7, MinKeep: 3}, fakeNow)

	// All 3 real dumps must be floor-protected. The only thing eligible for
	// deletion is a failure meta past cutoff — and none of the failure metas
	// here are older than 7 days, so nothing should be deleted at all.
	for _, v := range got {
		if classifyKind(v) == "dump" {
			t.Fatalf("a real dump was selected for deletion despite MinKeep=3: %s", v)
		}
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 victims (dumps floor-protected, failure metas recent), got %v", got)
	}
}

// TestSelectForDeletion_UnparseableDumpDoesNotConsumeFloor regresses the case
// where a foreign dump-shaped file with an unparseable timestamp
// (dump-manual.sql.gz.age) sorts lexically above all ISO timestamps (a letter
// outranks a digit) and — before the fix — consumed a MinKeep floor slot,
// pushing a real dump below the floor and letting the age sweep delete it.
func TestSelectForDeletion_UnparseableDumpDoesNotConsumeFloor(t *testing.T) {
	objs := []storage.Object{
		// Junk dump with a non-ISO "timestamp"; sorts first under reverse sort.
		{Path: "x/dump-manual.sql.gz.age", Size: 1},
	}
	// Exactly MinKeep real dumps, all older than the window.
	for _, age := range []int{20, 30, 40} {
		objs = append(objs, dump("x", fakeNow.AddDate(0, 0, -age))...)
	}

	got := selectForDeletion(objs, RetentionPolicy{Days: 7, MinKeep: 3}, fakeNow)

	// The junk file must be left alone, and all 3 real dumps must stay
	// floor-protected despite the junk sorting ahead of them.
	for _, v := range got {
		if classifyKind(v) == "dump" {
			t.Fatalf("a real dump was deleted; the junk file stole a floor slot: %s", v)
		}
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 victims, got %v", got)
	}
}

func TestClassifyKind(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"x/2026/01/01/dump-20260101T020000Z.sql.gz.age", "dump"},
		{"x/2026/01/01/dump-20260101T020000Z.meta.json", "meta"},
		{"x/random.txt", "other"},
		{"", "other"},
	}
	for _, c := range cases {
		if got := classifyKind(c.path); got != c.want {
			t.Errorf("classifyKind(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// recordingEmitter captures every Emit call so tests can assert on what
// Events the pipeline tried to write. Tests that don't care about Events
// keep using NoopEventEmitter.
type recordingEmitter struct {
	events []recordedEvent
}
type recordedEvent struct{ Type, Reason, Message string }

func (r *recordingEmitter) Emit(t, reason, msg string) {
	r.events = append(r.events, recordedEvent{t, reason, msg})
}

func TestRetention_EmitsRetentionFailedOnInitError(t *testing.T) {
	// An unsupported storage-type makes storageFactory.NewStorage fail
	// at the very first step inside retainForDestination — the
	// init-storage path. The new Event emission must surface that as a
	// Warning RetentionFailed Event with the phase tag, so the
	// cluster audit trail shows post-upload sweep failures even though
	// the meta.json was already uploaded by then.
	rec := &recordingEmitter{}
	p := &Pipeline{
		logger:         logr.Discard(),
		events:         rec,
		maxConcurrency: 1,
	}
	dests := []*secrets.Destination{
		{Name: "broken", StorageType: "definitely-not-a-real-type"},
	}
	results := p.applyRetention(context.Background(), dests, "prod-db", "post-upload",
		RetentionPolicy{Days: 30, MinKeep: 0}, fakeNow, p.logger)
	if len(results) != 1 || results[0].Status != meta.StatusFailed {
		t.Fatalf("expected 1 failed result, got %+v", results)
	}
	if len(rec.events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(rec.events), rec.events)
	}
	e := rec.events[0]
	if e.Type != "Warning" || e.Reason != "RetentionFailed" {
		t.Errorf("expected Warning/RetentionFailed, got %s/%s", e.Type, e.Reason)
	}
	if !strings.Contains(e.Message, "post-upload") {
		t.Errorf("event message must include phase, got %q", e.Message)
	}
	if !strings.Contains(e.Message, "prod-db") || !strings.Contains(e.Message, "broken") {
		t.Errorf("event message must include target and destination, got %q", e.Message)
	}
}

func TestRetention_DisabledEmitsNothing(t *testing.T) {
	rec := &recordingEmitter{}
	p := &Pipeline{logger: logr.Discard(), events: rec, maxConcurrency: 1}
	dests := []*secrets.Destination{{Name: "x", StorageType: "definitely-not-a-real-type"}}
	results := p.applyRetention(context.Background(), dests, "t", "post-upload",
		RetentionPolicy{Days: 0, MinKeep: 0}, fakeNow, p.logger)
	if results != nil {
		t.Errorf("disabled policy should return nil, got %+v", results)
	}
	if len(rec.events) != 0 {
		t.Errorf("disabled policy must not emit events, got %+v", rec.events)
	}
}

func TestSortDirsByDepth(t *testing.T) {
	dirs := map[string]bool{
		"target/2024":       true,
		"target/2024/01":    true,
		"target/2024/01/15": true,
		"target/2024/02":    true,
	}
	got := sortDirsByDepth(dirs)
	if len(got) != 4 {
		t.Fatalf("expected 4 dirs, got %d", len(got))
	}
	// Deepest first
	if got[0] != "target/2024/01/15" {
		t.Errorf("expected deepest first, got %s", got[0])
	}
	// Shallowest last
	if got[len(got)-1] != "target/2024" {
		t.Errorf("expected shallowest last, got %s", got[len(got)-1])
	}
}
