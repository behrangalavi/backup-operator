package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"backup-operator/storage"

	"github.com/go-logr/logr"
)

// fakePurgeStorage records Delete calls and can fail deletes matching failOn.
// objs is what List returns (listErr makes List fail instead).
type fakePurgeStorage struct {
	failOn  map[string]bool
	deleted []string
	objs    []storage.Object
	listErr error
}

func (f *fakePurgeStorage) Name() string { return "fake" }
func (f *fakePurgeStorage) Close() error { return nil }
func (f *fakePurgeStorage) Upload(context.Context, string, io.Reader) error {
	return errors.New("unused")
}
func (f *fakePurgeStorage) List(context.Context, string) ([]storage.Object, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.objs, nil
}
func (f *fakePurgeStorage) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}
func (f *fakePurgeStorage) Delete(_ context.Context, p string) error {
	if f.failOn[p] {
		return errors.New("permission denied")
	}
	f.deleted = append(f.deleted, p)
	return nil
}

func objsForPurge() []storage.Object {
	return []storage.Object{
		{Path: "x/2026/01/01/dump-20260101T020000Z.sql.gz.age", Size: 10},
		{Path: "x/2026/01/01/dump-20260101T020000Z.meta.json", Size: 2},
	}
}

// TestRunPurge_FailedDeleteReturnsNonZero regresses the bug where purge logged
// delete failures but still reported success and exited 0 — a right-to-erasure
// that silently leaves data behind.
func TestRunPurge_FailedDeleteReturnsNonZero(t *testing.T) {
	st := &fakePurgeStorage{failOn: map[string]bool{
		"x/2026/01/01/dump-20260101T020000Z.sql.gz.age": true,
	}}
	failed := runPurge(context.Background(), st, "fake", objsForPurge(), "x", time.Time{}, false, logr.Discard())
	if failed != 1 {
		t.Fatalf("expected 1 failed delete, got %d", failed)
	}
}

// TestRunPurge_AllSucceedReturnsZero confirms the happy path stays 0.
func TestRunPurge_AllSucceedReturnsZero(t *testing.T) {
	st := &fakePurgeStorage{}
	failed := runPurge(context.Background(), st, "fake", objsForPurge(), "x", time.Time{}, false, logr.Discard())
	if failed != 0 {
		t.Fatalf("expected 0 failures, got %d", failed)
	}
	if len(st.deleted) != 2 {
		t.Fatalf("expected 2 deletions, got %d", len(st.deleted))
	}
}

// TestRunPurge_DryRunNeverDeletes confirms dry-run touches nothing and is 0.
func TestRunPurge_DryRunNeverDeletes(t *testing.T) {
	st := &fakePurgeStorage{failOn: map[string]bool{"anything": true}}
	failed := runPurge(context.Background(), st, "fake", objsForPurge(), "x", time.Time{}, true, logr.Discard())
	if failed != 0 {
		t.Fatalf("dry-run must report 0 failures, got %d", failed)
	}
	if len(st.deleted) != 0 {
		t.Fatalf("dry-run must not delete anything, deleted %d", len(st.deleted))
	}
}

// TestRunPurge_CutoffKeepsNewer: --before only removes runs older than the
// cutoff; the newer run (and its sidecar) survives.
func TestRunPurge_CutoffKeepsNewer(t *testing.T) {
	st := &fakePurgeStorage{}
	objs := append(objsForPurge(),
		storage.Object{Path: "x/2026/03/01/dump-20260301T020000Z.sql.gz.age", Size: 10},
		storage.Object{Path: "x/2026/03/01/dump-20260301T020000Z.meta.json", Size: 2},
	)
	cutoff := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if failed := runPurge(context.Background(), st, "fake", objs, "x", cutoff, false, logr.Discard()); failed != 0 {
		t.Fatalf("failed=%d", failed)
	}
	if len(st.deleted) != 2 {
		t.Fatalf("expected only the January pair deleted, got %v", st.deleted)
	}
	for _, p := range st.deleted {
		if !containsStr(p, "20260101") {
			t.Errorf("deleted a run newer than the cutoff: %s", p)
		}
	}
}

// TestPurgeAll_EveryDestination regresses B1: a right-to-erasure must hit
// every destination given, and a failure on any one of them (including a
// destination that cannot be listed) makes the whole purge non-zero.
func TestPurgeAll_EveryDestination(t *testing.T) {
	a := &fakePurgeStorage{objs: objsForPurge()}
	b := &fakePurgeStorage{objs: objsForPurge()}
	stores := []destStorage{{name: "a", st: a}, {name: "b", st: b}}
	if failed := purgeAll(context.Background(), stores, "x", time.Time{}, false, logr.Discard()); failed != 0 {
		t.Fatalf("failed=%d", failed)
	}
	if len(a.deleted) != 2 || len(b.deleted) != 2 {
		t.Fatalf("both destinations must be purged: a=%d b=%d", len(a.deleted), len(b.deleted))
	}

	c := &fakePurgeStorage{objs: objsForPurge(), failOn: map[string]bool{"x/2026/01/01/dump-20260101T020000Z.meta.json": true}}
	d := &fakePurgeStorage{listErr: errors.New("connection refused")}
	stores = []destStorage{{name: "c", st: c}, {name: "d", st: d}}
	failed := purgeAll(context.Background(), stores, "x", time.Time{}, false, logr.Discard())
	if failed != 2 {
		t.Fatalf("expected 1 delete failure + 1 unlistable destination = 2, got %d", failed)
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
