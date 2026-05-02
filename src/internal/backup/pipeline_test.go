package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"backup-operator/internal/secrets"
	"backup-operator/storage"

	"github.com/go-logr/logr"
)

// --- mock storage for verifyUploadSize tests ---

type mockStorage struct {
	name    string
	objects []storage.Object
	listErr error
}

func (m *mockStorage) Name() string                                            { return m.name }
func (m *mockStorage) Upload(_ context.Context, _ string, _ io.Reader) error   { return nil }
func (m *mockStorage) Get(_ context.Context, _ string) (io.ReadCloser, error)  { return io.NopCloser(&bytes.Buffer{}), nil }
func (m *mockStorage) Delete(_ context.Context, _ string) error                { return nil }
func (m *mockStorage) List(_ context.Context, _ string) ([]storage.Object, error) {
	return m.objects, m.listErr
}

// --- verifyUploadSize tests ---

func TestVerifyUploadSize_Match(t *testing.T) {
	st := &mockStorage{
		objects: []storage.Object{
			{Path: "target/2026/01/01/dump-20260101T020000Z.sql.gz.age", Size: 12345},
		},
	}
	err := verifyUploadSize(context.Background(), st, "target/2026/01/01/dump-20260101T020000Z.sql.gz.age", 12345, logr.Discard())
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestVerifyUploadSize_Mismatch_RetryableError(t *testing.T) {
	st := &mockStorage{
		objects: []storage.Object{
			{Path: "target/2026/01/01/dump-20260101T020000Z.sql.gz.age", Size: 999},
		},
	}
	err := verifyUploadSize(context.Background(), st, "target/2026/01/01/dump-20260101T020000Z.sql.gz.age", 12345, logr.Discard())
	if err == nil {
		t.Fatal("expected error for size mismatch")
	}
	var re *RetryableError
	if !errors.As(err, &re) {
		t.Errorf("expected RetryableError, got %T: %v", err, err)
	}
	if re.Op != "upload verify" {
		t.Errorf("expected Op='upload verify', got %q", re.Op)
	}
}

func TestVerifyUploadSize_ListFails_Skips(t *testing.T) {
	st := &mockStorage{
		listErr: fmt.Errorf("connection refused"),
	}
	err := verifyUploadSize(context.Background(), st, "any/path", 100, logr.Discard())
	if err != nil {
		t.Fatalf("list failure should be skipped, got %v", err)
	}
}

func TestVerifyUploadSize_ObjectNotFound_Skips(t *testing.T) {
	st := &mockStorage{
		objects: []storage.Object{
			{Path: "other/path.txt", Size: 100},
		},
	}
	err := verifyUploadSize(context.Background(), st, "target/dump.sql.gz.age", 100, logr.Discard())
	if err != nil {
		t.Fatalf("object not found should be skipped, got %v", err)
	}
}

func TestVerifyUploadSize_PathSuffixMatch(t *testing.T) {
	st := &mockStorage{
		objects: []storage.Object{
			{Path: "prefix/target/2026/01/01/dump-20260101T020000Z.sql.gz.age", Size: 500},
		},
	}
	err := verifyUploadSize(context.Background(), st, "target/2026/01/01/dump-20260101T020000Z.sql.gz.age", 500, logr.Discard())
	if err != nil {
		t.Fatalf("suffix match should pass, got %v", err)
	}
}

// --- buildObjectPath tests ---

func TestBuildObjectPath_ValidTimestamp(t *testing.T) {
	got := buildObjectPath("prod-users", "20260501T020000Z", "sql.gz.age")
	want := "prod-users/2026/05/01/dump-20260501T020000Z.sql.gz.age"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildObjectPath_MetaJSON(t *testing.T) {
	got := buildObjectPath("prod-users", "20260501T020000Z", "meta.json")
	want := "prod-users/2026/05/01/dump-20260501T020000Z.meta.json"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildObjectPath_MalformedTimestamp(t *testing.T) {
	got := buildObjectPath("target", "not-a-timestamp", "sql.gz.age")
	want := "target/dump-not-a-timestamp.sql.gz.age"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// --- recoverGoroutine tests ---

func TestRecoverGoroutine_NoPanic(t *testing.T) {
	// Should not interfere when no panic occurs.
	done := make(chan bool, 1)
	go func() {
		defer recoverGoroutine(logr.Discard(), "test", "dest-1")
		done <- true
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("goroutine did not complete")
	}
}

func TestRecoverGoroutine_CatchesPanic(t *testing.T) {
	done := make(chan bool, 1)
	go func() {
		defer func() { done <- true }()
		defer recoverGoroutine(logr.Discard(), "upload", "dest-1")
		panic("simulated nil pointer")
	}()
	select {
	case <-done:
		// Goroutine completed without crashing the process.
	case <-time.After(time.Second):
		t.Fatal("goroutine did not complete after panic recovery")
	}
}

// --- sortedMetaPaths tests ---

func TestSortedMetaPaths_NewestFirst(t *testing.T) {
	objs := []storage.Object{
		{Path: "t/2026/01/01/dump-20260101T020000Z.meta.json", LastModified: time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)},
		{Path: "t/2026/01/02/dump-20260102T020000Z.meta.json", LastModified: time.Date(2026, 1, 2, 2, 0, 0, 0, time.UTC)},
		{Path: "t/2026/01/03/dump-20260103T020000Z.sql.gz.age", LastModified: time.Date(2026, 1, 3, 2, 0, 0, 0, time.UTC)},
	}
	got := sortedMetaPaths(objs)
	if len(got) != 2 {
		t.Fatalf("expected 2 meta paths, got %d", len(got))
	}
	if got[0] != objs[1].Path {
		t.Errorf("expected newest first: got %q, want %q", got[0], objs[1].Path)
	}
	if got[1] != objs[0].Path {
		t.Errorf("expected oldest second: got %q, want %q", got[1], objs[0].Path)
	}
}

func TestSortedMetaPaths_NoMetas(t *testing.T) {
	objs := []storage.Object{
		{Path: "t/dump.sql.gz.age"},
	}
	got := sortedMetaPaths(objs)
	if len(got) != 0 {
		t.Errorf("expected 0 meta paths, got %d", len(got))
	}
}

// --- metaJSON tests ---

func TestMetaJSON_SuccessStatus(t *testing.T) {
	src := testSource("prod-db", "postgres")
	meta := metaJSON(src, nil, nil, 42000, "abc123", "20260501T020000Z")
	if !bytes.Contains(meta, []byte(`"status": "success"`)) {
		t.Error("meta should contain status=success")
	}
	if !bytes.Contains(meta, []byte(`"sha256": "abc123"`)) {
		t.Error("meta should contain sha256")
	}
}

func TestFailureMetaJSON_FailedStatus(t *testing.T) {
	src := testSource("prod-db", "postgres")
	meta := failureMetaJSON(src, "20260501T020000Z", "dump", fmt.Errorf("pg_dump failed"))
	if !bytes.Contains(meta, []byte(`"status": "failed"`)) {
		t.Error("failure meta should contain status=failed")
	}
	if !bytes.Contains(meta, []byte(`"phase": "dump"`)) {
		t.Error("failure meta should contain phase")
	}
	if !bytes.Contains(meta, []byte(`pg_dump failed`)) {
		t.Error("failure meta should contain error message")
	}
}

// --- anonymization tests ---

func TestHashTableName_Deterministic(t *testing.T) {
	a := hashTableName("public.users")
	b := hashTableName("public.users")
	if a != b {
		t.Errorf("hashTableName not deterministic: %q vs %q", a, b)
	}
	c := hashTableName("public.orders")
	if a == c {
		t.Errorf("different tables should produce different hashes")
	}
}

// --- NoopEventEmitter tests ---

func TestNoopEventEmitter(t *testing.T) {
	e := NoopEventEmitter{}
	// Should not panic.
	e.Emit("Normal", "BackupStarted", "test")
	e.Emit("Warning", "BackupFailed", "test")
}

// --- helpers ---

func testSource(name, dbType string) *secrets.Source {
	return &secrets.Source{
		TargetName: name,
		DBType:     dbType,
	}
}
