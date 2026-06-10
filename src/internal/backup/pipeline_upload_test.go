package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"

	"backup-operator/internal/meta"
	"backup-operator/internal/secrets"
	"backup-operator/storage"

	"github.com/go-logr/logr"
)

// fakeUploadStorage records Upload/List calls and can be configured to fail a
// fixed number of initial uploads (transient) or to fail permanently. It
// reflects uploaded byte counts back through List so verifyUploadSize passes
// for successful uploads without a real backend.
type fakeUploadStorage struct {
	name string

	mu          sync.Mutex
	uploads     map[string]int   // path → attempt count
	sizes       map[string]int64 // path → last successfully-stored size
	failUploads int              // fail this many initial uploads, then succeed
	alwaysFail  bool
	failErr     error // error returned while failing; nil → generic (retryable)
}

func (f *fakeUploadStorage) Name() string { return f.name }

func (f *fakeUploadStorage) Upload(_ context.Context, p string, r io.Reader) error {
	b, _ := io.ReadAll(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.uploads == nil {
		f.uploads = make(map[string]int)
	}
	if f.sizes == nil {
		f.sizes = make(map[string]int64)
	}
	f.uploads[p]++
	if f.alwaysFail {
		if f.failErr != nil {
			return f.failErr
		}
		return fmt.Errorf("permission denied")
	}
	if f.failUploads > 0 {
		f.failUploads--
		if f.failErr != nil {
			return f.failErr
		}
		return fmt.Errorf("connection reset by peer")
	}
	f.sizes[p] = int64(len(b))
	return nil
}

func (f *fakeUploadStorage) List(_ context.Context, _ string) ([]storage.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	objs := make([]storage.Object, 0, len(f.sizes))
	for p, s := range f.sizes {
		objs = append(objs, storage.Object{Path: p, Size: s})
	}
	return objs, nil
}

func (f *fakeUploadStorage) Get(_ context.Context, _ string) (io.ReadCloser, error) {
	return io.NopCloser(&readerNop{}), nil
}
func (f *fakeUploadStorage) Delete(_ context.Context, _ string) error { return nil }

func (f *fakeUploadStorage) uploadCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.uploads[path]
}

type readerNop struct{}

func (readerNop) Read([]byte) (int, error) { return 0, io.EOF }

func newTestPipeline(t *testing.T, cache map[string]storage.Storage) *Pipeline {
	t.Helper()
	return &Pipeline{
		logger:         logr.Discard(),
		events:         NoopEventEmitter{},
		maxConcurrency: defaultMaxConcurrency,
		storageCache:   cache,
	}
}

func writeTempDump(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "dump-*.sql.gz.age")
	if err != nil {
		t.Fatalf("create temp dump: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write temp dump: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp dump: %v", err)
	}
	return f.Name()
}

func dest(name, storageType string) *secrets.Destination {
	return &secrets.Destination{Name: name, StorageType: storageType}
}

// --- fanOutDumps tests ---

func TestFanOutDumps_AllSucceed(t *testing.T) {
	a := &fakeUploadStorage{name: "hetzner"}
	b := &fakeUploadStorage{name: "aws-s3"}
	p := newTestPipeline(t, map[string]storage.Storage{"hetzner": a, "aws-s3": b})
	dumpFile := writeTempDump(t, "encrypted-dump-bytes")
	objectPath := "prod/2026/05/01/dump-x.sql.gz.age"

	results := p.fanOutDumps(context.Background(),
		[]*secrets.Destination{dest("hetzner", "sftp"), dest("aws-s3", "s3")},
		"prod", dumpFile, objectPath, logr.Discard())

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for _, r := range results {
		if r.Status != meta.StatusSuccess {
			t.Errorf("destination %s: expected success, got %s (err=%q)", r.Name, r.Status, r.Error)
		}
		if r.Error != "" {
			t.Errorf("destination %s: expected empty error, got %q", r.Name, r.Error)
		}
	}
	if a.uploadCount(objectPath) != 1 || b.uploadCount(objectPath) != 1 {
		t.Errorf("each destination should be uploaded once: hetzner=%d aws=%d",
			a.uploadCount(objectPath), b.uploadCount(objectPath))
	}
}

// TestFanOutDumps_PartialFailure proves a single bad destination does not
// abort its peers — the core "one destination down can't break all backups"
// contract. A permanent error (permission denied) avoids retry backoff.
func TestFanOutDumps_PartialFailure(t *testing.T) {
	good := &fakeUploadStorage{name: "hetzner"}
	bad := &fakeUploadStorage{name: "aws-s3", alwaysFail: true} // permission denied → permanent
	p := newTestPipeline(t, map[string]storage.Storage{"hetzner": good, "aws-s3": bad})
	dumpFile := writeTempDump(t, "encrypted-dump-bytes")
	objectPath := "prod/2026/05/01/dump-x.sql.gz.age"

	results := p.fanOutDumps(context.Background(),
		[]*secrets.Destination{dest("hetzner", "sftp"), dest("aws-s3", "s3")},
		"prod", dumpFile, objectPath, logr.Discard())

	byName := map[string]meta.DestinationResult{}
	for _, r := range results {
		byName[r.Name] = r
	}
	if byName["hetzner"].Status != meta.StatusSuccess {
		t.Errorf("hetzner should succeed, got %s", byName["hetzner"].Status)
	}
	if byName["aws-s3"].Status != meta.StatusFailed {
		t.Errorf("aws-s3 should fail, got %s", byName["aws-s3"].Status)
	}
	if byName["aws-s3"].Error == "" {
		t.Error("failed destination must carry an error string for the meta.json")
	}
	// Permanent error → single attempt, no retry.
	if got := bad.uploadCount(objectPath); got != 1 {
		t.Errorf("permanent error should not retry: got %d attempts", got)
	}
}

// --- uploadMeta tests ---

// TestUploadMeta_OnlySuccessfulDestinations confirms meta.json is written only
// to destinations whose dump upload succeeded — never to a destination that
// has no dump, which would create a "phantom backup" (meta points at a dump
// that isn't there).
func TestUploadMeta_OnlySuccessfulDestinations(t *testing.T) {
	good := &fakeUploadStorage{name: "hetzner"}
	bad := &fakeUploadStorage{name: "aws-s3"}
	p := newTestPipeline(t, map[string]storage.Storage{"hetzner": good, "aws-s3": bad})

	dests := []*secrets.Destination{dest("hetzner", "sftp"), dest("aws-s3", "s3")}
	results := []meta.DestinationResult{
		{Name: "hetzner", StorageType: "sftp", Status: meta.StatusSuccess},
		{Name: "aws-s3", StorageType: "s3", Status: meta.StatusFailed, Error: "dump upload failed"},
	}
	metaPath := "prod/2026/05/01/dump-x.meta.json"

	p.uploadMeta(context.Background(), dests, results, metaPath, []byte(`{"target":"prod"}`), logr.Discard())

	if good.uploadCount(metaPath) != 1 {
		t.Errorf("successful destination should receive meta exactly once, got %d", good.uploadCount(metaPath))
	}
	if bad.uploadCount(metaPath) != 0 {
		t.Errorf("failed destination must NOT receive meta, got %d uploads", bad.uploadCount(metaPath))
	}
}

// TestUploadMeta_RetriesTransientFailure proves the meta upload retries a
// transient failure rather than leaving a dump without its sidecar.
func TestUploadMeta_RetriesTransientFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("retry path sleeps on backoff")
	}
	st := &fakeUploadStorage{name: "hetzner", failUploads: 1} // fail once, then succeed
	p := newTestPipeline(t, map[string]storage.Storage{"hetzner": st})

	dests := []*secrets.Destination{dest("hetzner", "sftp")}
	results := []meta.DestinationResult{{Name: "hetzner", StorageType: "sftp", Status: meta.StatusSuccess}}
	metaPath := "prod/2026/05/01/dump-x.meta.json"

	p.uploadMeta(context.Background(), dests, results, metaPath, []byte(`{"target":"prod"}`), logr.Discard())

	if got := st.uploadCount(metaPath); got != 2 {
		t.Errorf("expected 1 failed + 1 successful attempt = 2, got %d", got)
	}
	st.mu.Lock()
	_, stored := st.sizes[metaPath]
	st.mu.Unlock()
	if !stored {
		t.Error("meta should be stored after the retry succeeded")
	}
}

// TestUploadMeta_CountsAllSucceeded: every eligible destination receives the
// sidecar → (attempted, succeeded) both equal the count of successful dumps.
func TestUploadMeta_CountsAllSucceeded(t *testing.T) {
	a := &fakeUploadStorage{name: "hetzner"}
	b := &fakeUploadStorage{name: "aws-s3"}
	p := newTestPipeline(t, map[string]storage.Storage{"hetzner": a, "aws-s3": b})
	dests := []*secrets.Destination{dest("hetzner", "sftp"), dest("aws-s3", "s3")}
	results := []meta.DestinationResult{
		{Name: "hetzner", Status: meta.StatusSuccess},
		{Name: "aws-s3", Status: meta.StatusSuccess},
	}
	attempted, succeeded := p.uploadMeta(context.Background(), dests, results, "p/dump.meta.json", []byte("{}"), logr.Discard())
	if attempted != 2 || succeeded != 2 {
		t.Fatalf("expected (2,2), got (%d,%d)", attempted, succeeded)
	}
}

// TestUploadMeta_OnlyCountsSuccessfulDumps: a destination whose DUMP failed is
// not eligible for meta upload, so it counts toward neither attempted nor
// succeeded.
func TestUploadMeta_OnlyCountsSuccessfulDumps(t *testing.T) {
	good := &fakeUploadStorage{name: "hetzner"}
	bad := &fakeUploadStorage{name: "aws-s3"}
	p := newTestPipeline(t, map[string]storage.Storage{"hetzner": good, "aws-s3": bad})
	dests := []*secrets.Destination{dest("hetzner", "sftp"), dest("aws-s3", "s3")}
	results := []meta.DestinationResult{
		{Name: "hetzner", Status: meta.StatusSuccess},
		{Name: "aws-s3", Status: meta.StatusFailed, Error: "dump upload failed"},
	}
	attempted, succeeded := p.uploadMeta(context.Background(), dests, results, "p/dump.meta.json", []byte("{}"), logr.Discard())
	if attempted != 1 || succeeded != 1 {
		t.Fatalf("expected (1,1) — only the successful-dump destination is eligible, got (%d,%d)", attempted, succeeded)
	}
}

// TestUploadMeta_PhantomBackupSignal: when every eligible destination fails the
// meta upload, succeeded==0 while attempted>0 — the signal Run() turns into a
// run failure so a dump-without-sidecar isn't silently reported as success.
func TestUploadMeta_PhantomBackupSignal(t *testing.T) {
	if testing.Short() {
		t.Skip("exercises the meta-upload retry backoff")
	}
	st := &fakeUploadStorage{name: "hetzner", alwaysFail: true} // never accepts the meta
	p := newTestPipeline(t, map[string]storage.Storage{"hetzner": st})
	dests := []*secrets.Destination{dest("hetzner", "sftp")}
	results := []meta.DestinationResult{{Name: "hetzner", Status: meta.StatusSuccess}}
	attempted, succeeded := p.uploadMeta(context.Background(), dests, results, "p/dump.meta.json", []byte("{}"), logr.Discard())
	if attempted != 1 || succeeded != 0 {
		t.Fatalf("expected (1,0) phantom signal, got (%d,%d)", attempted, succeeded)
	}
}

// --- uploadDumpWithRetry tests ---

func TestUploadDumpWithRetry_PermanentErrorNoRetry(t *testing.T) {
	st := &fakeUploadStorage{name: "aws-s3", alwaysFail: true} // permission denied
	p := newTestPipeline(t, map[string]storage.Storage{"aws-s3": st})
	dumpFile := writeTempDump(t, "bytes")
	objectPath := "prod/dump-x.sql.gz.age"

	err := p.uploadDumpWithRetry(context.Background(), dest("aws-s3", "s3"),
		"prod", dumpFile, objectPath, logr.Discard())
	if err == nil {
		t.Fatal("expected error for always-failing upload")
	}
	var perm *PermanentError
	if !errors.As(err, &perm) {
		t.Errorf("expected PermanentError, got %T: %v", err, err)
	}
	if got := st.uploadCount(objectPath); got != 1 {
		t.Errorf("permanent error must not retry: got %d attempts", got)
	}
}

func TestUploadDumpWithRetry_SuccessFirstTry(t *testing.T) {
	st := &fakeUploadStorage{name: "hetzner"}
	p := newTestPipeline(t, map[string]storage.Storage{"hetzner": st})
	dumpFile := writeTempDump(t, "bytes")
	objectPath := "prod/dump-x.sql.gz.age"

	err := p.uploadDumpWithRetry(context.Background(), dest("hetzner", "sftp"),
		"prod", dumpFile, objectPath, logr.Discard())
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if got := st.uploadCount(objectPath); got != 1 {
		t.Errorf("expected exactly 1 upload, got %d", got)
	}
}
