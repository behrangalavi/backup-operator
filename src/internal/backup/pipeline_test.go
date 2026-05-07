package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"backup-operator/analyzer"
	"backup-operator/dumper"
	"backup-operator/internal/meta"
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
	m, err := metaJSON(src, nil, "", nil, nil, 42000, "abc123", "20260501T020000Z", time.Time{}, time.Time{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("metaJSON: %v", err)
	}
	if !bytes.Contains(m, []byte(`"status": "success"`)) {
		t.Error("meta should contain status=success")
	}
	if !bytes.Contains(m, []byte(`"sha256": "abc123"`)) {
		t.Error("meta should contain sha256")
	}
	if bytes.Contains(m, []byte(`"statsError"`)) {
		t.Error("empty statsError must be omitted from JSON, not serialised as \"\"")
	}
}

func TestMetaJSON_StatsErrorPresent(t *testing.T) {
	src := testSource("prod-db", "postgres")
	m, err := metaJSON(src, nil, `connect: failed to connect to "postgres://app:***@db:5432/app": permission denied`,
		nil, nil, 42000, "abc123", "20260501T020000Z", time.Time{}, time.Time{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("metaJSON: %v", err)
	}
	if !bytes.Contains(m, []byte(`"statsError":`)) {
		t.Error("non-empty statsError must be serialised")
	}
	if !bytes.Contains(m, []byte(`permission denied`)) {
		t.Error("statsError content should round-trip into the meta JSON")
	}
}

func TestMetaJSON_WithDestinations(t *testing.T) {
	src := testSource("prod-db", "postgres")
	drs := []meta.DestinationResult{
		{Name: "hetzner", StorageType: "sftp", Status: meta.StatusSuccess},
		{Name: "aws-s3", StorageType: "s3", Status: meta.StatusFailed, Error: "connection refused"},
	}
	m, err := metaJSON(src, nil, "", nil, nil, 42000, "abc123", "20260501T020000Z", time.Time{}, time.Time{}, drs, nil, nil)
	if err != nil {
		t.Fatalf("metaJSON: %v", err)
	}
	if !bytes.Contains(m, []byte(`"destinations"`)) {
		t.Error("meta should contain destinations array")
	}
	if !bytes.Contains(m, []byte(`"hetzner"`)) {
		t.Error("meta should contain hetzner destination")
	}
	if !bytes.Contains(m, []byte(`"connection refused"`)) {
		t.Error("meta should contain error for failed destination")
	}
}

func TestMetaJSON_WithRetention(t *testing.T) {
	src := testSource("prod-db", "postgres")
	rr := []meta.RetentionResult{
		{Name: "hetzner", Status: meta.StatusSuccess, DeletedDumps: 2, DeletedMetas: 2},
		{Name: "aws-s3", Status: meta.StatusFailed, Error: "list: connection refused"},
	}
	m, err := metaJSON(src, nil, "", nil, nil, 42000, "abc123", "20260501T020000Z", time.Time{}, time.Time{}, nil, nil, rr)
	if err != nil {
		t.Fatalf("metaJSON: %v", err)
	}
	if !bytes.Contains(m, []byte(`"retention"`)) {
		t.Error("meta should contain retention block")
	}
	if !bytes.Contains(m, []byte(`"deletedDumps": 2`)) {
		t.Error("meta should record dump deletions")
	}
	if !bytes.Contains(m, []byte(`"list: connection refused"`)) {
		t.Error("meta should contain retention error string")
	}
}

func TestFailureMetaJSON_FailedStatus(t *testing.T) {
	src := testSource("prod-db", "postgres")
	meta, err := failureMetaJSON(src, "20260501T020000Z", "dump", time.Time{}, fmt.Errorf("pg_dump failed"))
	if err != nil {
		t.Fatalf("failureMetaJSON: %v", err)
	}
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

func TestFallbackMetaJSON_ValidParseableJSON(t *testing.T) {
	// fallbackMetaJSON is the safety net when MarshalIndent of the full
	// MetaFile fails. The output must be parseable as a MetaFile and must
	// carry enough information for the UI/refresher to surface the run.
	body := fallbackMetaJSON("prod-db", "20260501T020000Z", "postgres", "meta-marshal", fmt.Errorf("synthetic"))
	if len(body) == 0 {
		t.Fatal("fallback must never return empty bytes")
	}
	var parsed meta.MetaFile
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("fallback meta must be valid JSON: %v\n%s", err, body)
	}
	if parsed.Target != "prod-db" {
		t.Errorf("Target round-trip: got %q", parsed.Target)
	}
	if parsed.Status != meta.StatusFailed {
		t.Errorf("Status must be failed in fallback, got %q", parsed.Status)
	}
	if parsed.Phase != "meta-marshal" {
		t.Errorf("Phase round-trip: got %q", parsed.Phase)
	}
	if !bytes.Contains(body, []byte("synthetic")) {
		t.Error("fallback should preserve original marshal error message")
	}
}

func TestMetaJSON_WithRestoreVerification(t *testing.T) {
	src := testSource("prod-db", "postgres")
	rv := &meta.RestoreVerificationResult{
		Mode:                          "stream-validate",
		Verdict:                       meta.VerificationMatch,
		Summary:                       "decrypt + parse OK; 100 rows",
		EphemeralRecipientFingerprint: "abc1234567890def",
		DurationSeconds:               1.23,
	}
	m, err := metaJSON(src, nil, "", nil, nil, 42000, "abc123", "20260501T020000Z", time.Time{}, time.Time{}, nil, rv, nil)
	if err != nil {
		t.Fatalf("metaJSON: %v", err)
	}
	if !bytes.Contains(m, []byte(`"restoreVerification"`)) {
		t.Error("meta should contain restoreVerification")
	}
	if !bytes.Contains(m, []byte(`"stream-validate"`)) {
		t.Error("meta should contain mode")
	}
	if !bytes.Contains(m, []byte(`"abc1234567890def"`)) {
		t.Error("meta should contain ephemeral fingerprint")
	}
}

func TestMetaJSON_WithVerification(t *testing.T) {
	src := testSource("prod-db", "postgres")
	v := &meta.DumpVerification{
		Verdict: meta.VerificationMatch,
		Summary: "all 3 tables verified",
		Tables: []meta.TableVerification{
			{Name: "users", PreDumpRows: 100, PostDumpRows: 100, DumpRows: 100, Verdict: "match"},
		},
	}
	m, err := metaJSON(src, nil, "", nil, v, 42000, "abc123", "20260501T020000Z", time.Time{}, time.Time{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("metaJSON: %v", err)
	}
	if !bytes.Contains(m, []byte(`"verification"`)) {
		t.Error("meta should contain verification")
	}
	if !bytes.Contains(m, []byte(`"match"`)) {
		t.Error("meta should contain match verdict")
	}
	if !bytes.Contains(m, []byte(`"all 3 tables verified"`)) {
		t.Error("meta should contain verification summary")
	}
}

// --- anonymization tests ---

// TestAnonymizeTables_NoFalsePositiveDisappeared regresses the bug where
// AnonymizeTables=true caused every previous-run table to flag as
// "table-disappeared" on every subsequent run. Root cause: the previous
// meta's Stats already had hashed names, but the current run's stats had
// real names, so the analyzer's name-keyed map never matched. Fix:
// pipeline hashes the current stats (cmpStats) before passing to Compare
// when AnonymizeTables is on.
func TestAnonymizeTables_NoFalsePositiveDisappeared(t *testing.T) {
	realStats := &dumper.Stats{
		SchemaHash: "abc",
		Tables: []dumper.TableStats{
			{Name: "public.users", RowCount: 100, SizeBytes: 1024},
			{Name: "public.orders", RowCount: 50, SizeBytes: 512},
		},
	}
	// What gets persisted into prev meta.json when AnonymizeTables=true.
	prevPersisted := anonymizeStats(realStats)
	// Same DB on the next run — same real names, same row counts. No drift.
	currReal := &dumper.Stats{
		SchemaHash: "abc",
		Tables: []dumper.TableStats{
			{Name: "public.users", RowCount: 100, SizeBytes: 1024},
			{Name: "public.orders", RowCount: 50, SizeBytes: 512},
		},
	}
	// What the pipeline now feeds the analyzer instead of currReal.
	cmpStats := anonymizeStats(currReal)

	a := analyzer.NewAnalyzer()
	report := a.Compare(prevPersisted, cmpStats, 1000, 1000)

	for _, an := range report.Anomalies {
		if an.Kind == "table-disappeared" {
			t.Errorf("unexpected table-disappeared anomaly for %q — anonymized prev should match anonymized curr",
				an.Subject)
		}
	}
	if len(report.Anomalies) != 0 {
		t.Errorf("expected zero anomalies for unchanged DB, got %d: %+v",
			len(report.Anomalies), report.Anomalies)
	}
}

// TestAnonymizeTables_TransitionRunNoFalsePositives regresses the case
// where a user freshly toggles anonymize-tables=true: the last meta.json
// holds real names, the new run hashes its own stats, and the analyzer
// would otherwise see every prev table as "disappeared" for one run.
// The pipeline must detect that prev is still in real-name form and
// hash it before Compare.
func TestAnonymizeTables_TransitionRunNoFalsePositives(t *testing.T) {
	prevReal := &dumper.Stats{
		SchemaHash: "abc",
		Tables: []dumper.TableStats{
			{Name: "platform.users", RowCount: 100, SizeBytes: 1024},
			{Name: "platform.orders", RowCount: 50, SizeBytes: 512},
		},
	}
	currReal := &dumper.Stats{
		SchemaHash: "abc",
		Tables: []dumper.TableStats{
			{Name: "platform.users", RowCount: 100, SizeBytes: 1024},
			{Name: "platform.orders", RowCount: 50, SizeBytes: 512},
		},
	}
	if looksAnonymized(prevReal) {
		t.Fatalf("prevReal must not look anonymized")
	}
	cmpStats := anonymizeStats(currReal)
	cmpPrev := anonymizeStats(prevReal)

	report := analyzer.NewAnalyzer().Compare(cmpPrev, cmpStats, 1000, 1000)
	if len(report.Anomalies) != 0 {
		t.Errorf("expected zero anomalies on transition run, got %d: %+v",
			len(report.Anomalies), report.Anomalies)
	}
}

// TestAnonymizeTables_OffTransitionSuppressesTableComparison regresses the
// reverse-direction toggle: anonymize-tables was previously on (prev meta
// is hashed) and the user just turned it off. The per-table comparison
// cannot be reconciled — hashes are one-way. The pipeline must clear prev's
// Tables for this run so no `table-disappeared` anomalies fire, while
// preserving schema-hash, charset and size signals that don't depend on
// table names.
func TestAnonymizeTables_OffTransitionSuppressesTableComparison(t *testing.T) {
	prevHashed := &dumper.Stats{
		SchemaHash: "abc",
		Charset:    "utf8mb4",
		Tables: []dumper.TableStats{
			{Name: hashTableName("public.users"), RowCount: 100, SizeBytes: 1024},
			{Name: hashTableName("public.orders"), RowCount: 50, SizeBytes: 512},
		},
	}
	currReal := &dumper.Stats{
		SchemaHash: "abc",
		Charset:    "utf8mb4",
		Tables: []dumper.TableStats{
			{Name: "public.users", RowCount: 100, SizeBytes: 1024},
			{Name: "public.orders", RowCount: 50, SizeBytes: 512},
		},
	}
	if !looksAnonymized(prevHashed) {
		t.Fatalf("prevHashed must look anonymized")
	}
	// Mirror the pipeline's reverse-transition handling.
	cleared := *prevHashed
	cleared.Tables = nil
	cmpPrev := &cleared

	report := analyzer.NewAnalyzer().Compare(cmpPrev, currReal, 1000, 1000)
	if len(report.Anomalies) != 0 {
		t.Errorf("expected zero anomalies on off-transition run, got %d: %+v",
			len(report.Anomalies), report.Anomalies)
	}
	if report.SchemaChanged {
		t.Errorf("schema-hash drift detection must survive table clearing")
	}
	if report.CharsetChanged {
		t.Errorf("charset drift detection must survive table clearing")
	}
}

// TestAnonymizeTables_OffTransitionStillFlagsRealDrift confirms that
// suppressing the table comparison does not also suppress schema, charset
// or size signals on the same off-transition run.
func TestAnonymizeTables_OffTransitionStillFlagsRealDrift(t *testing.T) {
	prevHashed := &dumper.Stats{
		SchemaHash: "abc",
		Charset:    "utf8",
		Tables: []dumper.TableStats{
			{Name: hashTableName("public.users")},
		},
	}
	currReal := &dumper.Stats{
		SchemaHash: "DEF",
		Charset:    "utf8mb4",
		Tables: []dumper.TableStats{
			{Name: "public.users"},
		},
	}
	cleared := *prevHashed
	cleared.Tables = nil
	cmpPrev := &cleared

	report := analyzer.NewAnalyzer().Compare(cmpPrev, currReal, 10000, 100)
	if !report.SchemaChanged {
		t.Errorf("expected SchemaChanged=true")
	}
	if !report.CharsetChanged {
		t.Errorf("expected CharsetChanged=true")
	}
	hasSize := false
	for _, a := range report.Anomalies {
		if a.Kind == "size-collapse" {
			hasSize = true
			break
		}
	}
	if !hasSize {
		t.Errorf("expected size-collapse anomaly, got %+v", report.Anomalies)
	}
}

func TestLooksAnonymized(t *testing.T) {
	cases := []struct {
		name string
		in   *dumper.Stats
		want bool
	}{
		{"nil", nil, true},
		{"empty", &dumper.Stats{}, true},
		{"all hashed", &dumper.Stats{Tables: []dumper.TableStats{
			{Name: hashTableName("public.users")},
			{Name: hashTableName("public.orders")},
		}}, true},
		{"all real", &dumper.Stats{Tables: []dumper.TableStats{
			{Name: "public.users"},
			{Name: "public.orders"},
		}}, false},
		{"mixed", &dumper.Stats{Tables: []dumper.TableStats{
			{Name: hashTableName("public.users")},
			{Name: "public.orders"},
		}}, false},
		{"non-hex 16-char", &dumper.Stats{Tables: []dumper.TableStats{
			{Name: "public.zzzzzzzzz"},
		}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looksAnonymized(c.in); got != c.want {
				t.Errorf("looksAnonymized: got %v want %v", got, c.want)
			}
		})
	}
}

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
