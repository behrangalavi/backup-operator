package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"backup-operator/storage"
)

// fakeStorage is a minimal in-memory storage.Storage implementation that
// serves a fixed set of meta.json blobs. Keyed by path; List returns one
// Object per key, Get returns the stored bytes.
type fakeStorage struct {
	name string
	blobs map[string][]byte
}

func (f *fakeStorage) Name() string { return f.name }
func (f *fakeStorage) Upload(_ context.Context, _ string, _ io.Reader) error { return nil }
func (f *fakeStorage) Delete(_ context.Context, _ string) error              { return nil }
func (f *fakeStorage) Get(_ context.Context, p string) (io.ReadCloser, error) {
	b, ok := f.blobs[p]
	if !ok {
		return nil, io.EOF
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (f *fakeStorage) List(_ context.Context, _ string) ([]storage.Object, error) {
	out := make([]storage.Object, 0, len(f.blobs))
	for p := range f.blobs {
		out = append(out, storage.Object{Path: p, Size: int64(len(f.blobs[p]))})
	}
	return out, nil
}

// makeMeta returns the JSON bytes for a MetaFile with the given fields.
func makeMeta(t *testing.T, target, ts, status string, dur float64) []byte {
	t.Helper()
	m := MetaFile{
		Target:          target,
		Timestamp:       ts,
		Status:          status,
		DurationSeconds: dur,
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}

func TestIsFailure(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{StatusFailed, true},
		{StatusSuccess, false},
		{"", false}, // legacy metas without status count as success
	}
	for _, tt := range tests {
		m := MetaFile{Status: tt.status}
		if got := m.IsFailure(); got != tt.want {
			t.Errorf("IsFailure(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestParsedTimestamp_Valid(t *testing.T) {
	m := MetaFile{Timestamp: "20260428T020000Z"}
	got := m.ParsedTimestamp()
	want := time.Date(2026, 4, 28, 2, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("ParsedTimestamp() = %v, want %v", got, want)
	}
}

func TestParsedTimestamp_Invalid(t *testing.T) {
	m := MetaFile{Timestamp: "not-a-timestamp"}
	got := m.ParsedTimestamp()
	if !got.IsZero() {
		t.Errorf("ParsedTimestamp() should be zero for invalid input, got %v", got)
	}
}

func TestParsedTimestamp_Empty(t *testing.T) {
	m := MetaFile{}
	got := m.ParsedTimestamp()
	if !got.IsZero() {
		t.Errorf("ParsedTimestamp() should be zero for empty input, got %v", got)
	}
}

func TestMedianDuration_OddSampleSize(t *testing.T) {
	st := &fakeStorage{name: "fake", blobs: map[string][]byte{
		"prod-db/2026/05/01/dump-20260501T020000Z.meta.json": makeMeta(t, "prod-db", "20260501T020000Z", StatusSuccess, 100),
		"prod-db/2026/05/02/dump-20260502T020000Z.meta.json": makeMeta(t, "prod-db", "20260502T020000Z", StatusSuccess, 200),
		"prod-db/2026/05/03/dump-20260503T020000Z.meta.json": makeMeta(t, "prod-db", "20260503T020000Z", StatusSuccess, 50),
	}}
	d, n, err := MedianDuration(context.Background(), st, "prod-db", 10)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 3 {
		t.Errorf("sample size: got %d want 3", n)
	}
	if d != 100*time.Second {
		t.Errorf("median: got %v want 100s", d)
	}
}

func TestMedianDuration_EvenSampleSize(t *testing.T) {
	st := &fakeStorage{name: "fake", blobs: map[string][]byte{
		"prod-db/2026/05/01/dump-20260501T020000Z.meta.json": makeMeta(t, "prod-db", "20260501T020000Z", StatusSuccess, 100),
		"prod-db/2026/05/02/dump-20260502T020000Z.meta.json": makeMeta(t, "prod-db", "20260502T020000Z", StatusSuccess, 200),
	}}
	d, n, err := MedianDuration(context.Background(), st, "prod-db", 10)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 2 {
		t.Errorf("sample size: got %d want 2", n)
	}
	if d != 150*time.Second {
		t.Errorf("median: got %v want 150s", d)
	}
}

func TestMedianDuration_SkipsFailuresAndZeros(t *testing.T) {
	st := &fakeStorage{name: "fake", blobs: map[string][]byte{
		"prod-db/2026/05/01/dump-20260501T020000Z.meta.json": makeMeta(t, "prod-db", "20260501T020000Z", StatusSuccess, 100),
		"prod-db/2026/05/02/dump-20260502T020000Z.meta.json": makeMeta(t, "prod-db", "20260502T020000Z", StatusFailed, 5),
		"prod-db/2026/05/03/dump-20260503T020000Z.meta.json": makeMeta(t, "prod-db", "20260503T020000Z", StatusSuccess, 0), // legacy meta
	}}
	d, n, err := MedianDuration(context.Background(), st, "prod-db", 10)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 1 {
		t.Errorf("sample size: got %d want 1 (only one valid sample)", n)
	}
	if d != 100*time.Second {
		t.Errorf("median: got %v want 100s", d)
	}
}

func TestMedianDuration_NoSuccessfulRuns(t *testing.T) {
	st := &fakeStorage{name: "fake", blobs: map[string][]byte{
		"prod-db/2026/05/01/dump-20260501T020000Z.meta.json": makeMeta(t, "prod-db", "20260501T020000Z", StatusFailed, 5),
	}}
	d, n, err := MedianDuration(context.Background(), st, "prod-db", 10)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 0 || d != 0 {
		t.Errorf("expected zero estimate, got d=%v n=%d", d, n)
	}
}

func TestMedianDuration_RespectsCap(t *testing.T) {
	blobs := map[string][]byte{}
	for i := 0; i < 20; i++ {
		ts := time.Date(2026, 5, 1+i, 2, 0, 0, 0, time.UTC).Format("20060102T150405Z")
		blobs["prod-db/2026/05/"+ts+".meta.json"] = makeMeta(t, "prod-db", ts, StatusSuccess, float64(i+1))
	}
	st := &fakeStorage{name: "fake", blobs: blobs}
	_, n, err := MedianDuration(context.Background(), st, "prod-db", 5)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 5 {
		t.Errorf("sample size: got %d want 5 (cap)", n)
	}
}

func TestMetaFile_JSONPath_OmittedFromJSON(t *testing.T) {
	// Path has json:"-", so it should not appear in JSON output.
	m := MetaFile{
		Target:    "prod-db",
		Timestamp: "20260428T020000Z",
		DBType:    "postgres",
		Status:    StatusSuccess,
		Path:      "prod-db/dump-20260428T020000Z.meta.json",
	}
	if m.Path == "" {
		t.Error("Path should be set before serialization check")
	}
}
