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

	mu           sync.Mutex
	uploads      map[string]int   // path → attempt count
	sizes        map[string]int64 // path → last successfully-stored size
	failUploads  int              // fail this many initial uploads, then succeed
	alwaysFail   bool
	failErr      error            // error returned while failing; nil → generic (retryable)
	listErr      error            // when set, List fails with it
	hideFromList bool             // when set, List omits the uploaded objects
	listExtra    []storage.Object // appended to every listing (sibling objects)
}

func (f *fakeUploadStorage) Name() string { return f.name }
func (f *fakeUploadStorage) Close() error { return nil }

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
	if f.listErr != nil {
		return nil, f.listErr
	}
	objs := make([]storage.Object, 0, len(f.sizes)+len(f.listExtra))
	if !f.hideFromList {
		for p, s := range f.sizes {
			objs = append(objs, storage.Object{Path: p, Size: s})
		}
	}
	objs = append(objs, f.listExtra...)
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

// uploadEventRecorder captures Events so tests can assert on reasons.
type uploadEventRecorder struct {
	mu     sync.Mutex
	events []string // "Type/Reason"
}

func (r *uploadEventRecorder) Emit(typ, reason, _ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, typ+"/"+reason)
}

func (r *uploadEventRecorder) has(s string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e == s {
			return true
		}
	}
	return false
}

// TestFanOutDumps_RedactsErrorInMeta regresses C1: the per-destination error
// string lands in the plaintext meta.json, so credentials embedded in a
// storage error (URI userinfo, key=value) must be scrubbed first.
func TestFanOutDumps_RedactsErrorInMeta(t *testing.T) {
	bad := &fakeUploadStorage{name: "s3", alwaysFail: true,
		failErr: fmt.Errorf("permission denied: PUT https://AKIA123:verysecret@bucket.example/x failed, secret-access-key=verysecret")}
	p := newTestPipeline(t, map[string]storage.Storage{"s3": bad})
	dumpFile := writeTempDump(t, "bytes")

	results := p.fanOutDumps(context.Background(), []*secrets.Destination{dest("s3", "s3")},
		"prod", dumpFile, "prod/dump-x.sql.gz.age", logr.Discard())

	if results[0].Status != meta.StatusFailed || results[0].Error == "" {
		t.Fatalf("expected a failed result with an error, got %+v", results[0])
	}
	for _, leak := range []string{"verysecret", "AKIA123"} {
		if containsText(results[0].Error, leak) {
			t.Errorf("meta error must not contain %q: %q", leak, results[0].Error)
		}
	}
	if !containsText(results[0].Error, "permission denied") {
		t.Errorf("diagnostic part of the error must survive redaction: %q", results[0].Error)
	}
}

// TestVerifyUploadSize_ExactMatchAndSkips covers the round-5 finding: the
// match is exact on the object path (a sibling with the same basename under
// a different prefix is ignored), and when the check cannot run the run
// continues but a Warning UploadVerifySkipped Event is emitted.
func TestVerifyUploadSize_ExactMatchAndSkips(t *testing.T) {
	objectPath := "prod/2026/05/01/dump-x.sql.gz.age"
	mk := func(st *fakeUploadStorage) (*Pipeline, *uploadEventRecorder) {
		rec := &uploadEventRecorder{}
		p := newTestPipeline(t, map[string]storage.Storage{"d": st})
		p.events = rec
		return p, rec
	}

	// 1. Exact match with the right size → passes, no event.
	st := &fakeUploadStorage{name: "d"}
	_ = st.Upload(context.Background(), objectPath, bytesReader("12345"))
	p, rec := mk(st)
	if err := p.verifyUploadSize(context.Background(), st, "d", "prod", objectPath, 5); err != nil {
		t.Fatalf("exact match must pass: %v", err)
	}
	if rec.has("Warning/UploadVerifySkipped") {
		t.Error("a successful verification must not emit a skip event")
	}

	// 2. Exact match with the wrong size → RetryableError.
	p, _ = mk(st)
	err := p.verifyUploadSize(context.Background(), st, "d", "prod", objectPath, 99)
	var retryable *RetryableError
	if !errors.As(err, &retryable) {
		t.Fatalf("size mismatch must be a RetryableError, got %v", err)
	}

	// 3. Only a same-basename sibling under another prefix is listed → the old
	//    loose suffix match would have compared against it; now it's a skip.
	sibling := &fakeUploadStorage{name: "d", hideFromList: true,
		listExtra: []storage.Object{{Path: "other/2026/05/01/dump-x.sql.gz.age", Size: 99}}}
	p, rec = mk(sibling)
	if err := p.verifyUploadSize(context.Background(), sibling, "d", "prod", objectPath, 5); err != nil {
		t.Fatalf("a sibling object must not be compared: %v", err)
	}
	if !rec.has("Warning/UploadVerifySkipped") {
		t.Error("object missing from listing must emit UploadVerifySkipped")
	}

	// 4. List fails → skip with event, run continues.
	broken := &fakeUploadStorage{name: "d", listErr: errors.New("550 not permitted for user:pw@host")}
	p, rec = mk(broken)
	if err := p.verifyUploadSize(context.Background(), broken, "d", "prod", objectPath, 5); err != nil {
		t.Fatalf("list failure must not fail the upload: %v", err)
	}
	if !rec.has("Warning/UploadVerifySkipped") {
		t.Error("list failure must emit UploadVerifySkipped")
	}
}

func containsText(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

type strReader struct {
	s string
	i int
}

func (r *strReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}

func bytesReader(s string) io.Reader { return &strReader{s: s} }
