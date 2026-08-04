package backup

import (
	"testing"
	"time"

	"backup-operator/storage"
)

// TestSortedMetaPaths_OrdersByPathTimestampNotMtime regresses the baseline bug:
// object mtime is unreliable, so ordering must come from the ISO timestamp in
// the path. Here the OLDEST run carries the NEWEST LastModified (as a
// replicated/mtime-bumped backend would report) — the sort must still put the
// chronologically newest run first.
func TestSortedMetaPaths_OrdersByPathTimestampNotMtime(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	objs := []storage.Object{
		{Path: "t/2026/01/01/dump-20260101T020000Z.meta.json", LastModified: base.Add(72 * time.Hour)},
		{Path: "t/2026/01/03/dump-20260103T020000Z.meta.json", LastModified: base.Add(1 * time.Hour)},
		{Path: "t/2026/01/02/dump-20260102T020000Z.meta.json", LastModified: base.Add(48 * time.Hour)},
		{Path: "t/2026/01/03/dump-20260103T020000Z.sql.gz.age", LastModified: base}, // not a meta
	}
	got := sortedMetaPaths(objs)
	want := []string{
		"t/2026/01/03/dump-20260103T020000Z.meta.json",
		"t/2026/01/02/dump-20260102T020000Z.meta.json",
		"t/2026/01/01/dump-20260101T020000Z.meta.json",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d paths, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d = %q, want %q", i, got[i], want[i])
		}
	}
}
