package main

import (
	"context"
	"errors"
	"io"
	"testing"

	"backup-operator/storage"

	"github.com/go-logr/logr"
)

// fakePurgeStorage records Delete calls and can fail deletes matching failOn.
type fakePurgeStorage struct {
	failOn  map[string]bool
	deleted []string
}

func (f *fakePurgeStorage) Name() string { return "fake" }
func (f *fakePurgeStorage) Upload(context.Context, string, io.Reader) error {
	return errors.New("unused")
}
func (f *fakePurgeStorage) List(context.Context, string) ([]storage.Object, error) {
	return nil, errors.New("unused")
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
	failed := runPurge(context.Background(), st, objsForPurge(), "x", "", false, logr.Discard())
	if failed != 1 {
		t.Fatalf("expected 1 failed delete, got %d", failed)
	}
}

// TestRunPurge_AllSucceedReturnsZero confirms the happy path stays 0.
func TestRunPurge_AllSucceedReturnsZero(t *testing.T) {
	st := &fakePurgeStorage{}
	failed := runPurge(context.Background(), st, objsForPurge(), "x", "", false, logr.Discard())
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
	failed := runPurge(context.Background(), st, objsForPurge(), "x", "", true, logr.Discard())
	if failed != 0 {
		t.Fatalf("dry-run must report 0 failures, got %d", failed)
	}
	if len(st.deleted) != 0 {
		t.Fatalf("dry-run must not delete anything, deleted %d", len(st.deleted))
	}
}
