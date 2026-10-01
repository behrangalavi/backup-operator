package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"backup-operator/internal/labels"
	"backup-operator/internal/meta"
	"backup-operator/storage"

	"github.com/go-logr/logr"
)

// fakeGetStorage serves fixed object bodies; Get on an unknown path fails.
type fakeGetStorage struct {
	bodies map[string]string
}

func (f *fakeGetStorage) Name() string { return "fake" }
func (f *fakeGetStorage) Close() error { return nil }
func (f *fakeGetStorage) Upload(context.Context, string, io.Reader) error {
	return errors.New("unused")
}
func (f *fakeGetStorage) List(context.Context, string) ([]storage.Object, error) {
	return nil, errors.New("unused")
}
func (f *fakeGetStorage) Delete(context.Context, string) error { return errors.New("unused") }
func (f *fakeGetStorage) Get(_ context.Context, p string) (io.ReadCloser, error) {
	b, ok := f.bodies[p]
	if !ok {
		return nil, errors.New("not found")
	}
	return io.NopCloser(strings.NewReader(b)), nil
}

func obj(p string) storage.Object { return storage.Object{Path: p, Size: int64(len(p))} }

// --- D2: pickDumps ---

func TestPickDumps_RecognisesEverySuffixAndSkipsTheRest(t *testing.T) {
	objs := []storage.Object{
		obj("t/2026/01/01/dump-20260101T020000Z.sql.gz.age"),
		obj("t/2026/01/02/dump-20260102T020000Z.sql.zst.age"),
		obj("t/2026/01/02/dump-20260102T020000Z.meta.json"),   // sidecar, not a dump
		obj("t/2026/01/03/notes-20260103T020000Z.sql.gz.age"), // wrong prefix
		obj("t/2026/01/04/dump-20260104T020000Z.sql.gz"),      // not encrypted
		obj("t/2026/01/05/dump-manual.sql.gz.age"),            // odd timestamp still listed
	}
	got := pickDumps(objs)
	want := map[string]string{
		"20260101T020000Z": "t/2026/01/01/dump-20260101T020000Z.sql.gz.age",
		"20260102T020000Z": "t/2026/01/02/dump-20260102T020000Z.sql.zst.age",
		"manual":           "t/2026/01/05/dump-manual.sql.gz.age",
	}
	if len(got) != len(want) {
		t.Fatalf("pickDumps = %+v, want %d entries", got, len(want))
	}
	for _, d := range got {
		if want[d.timestamp] != d.path {
			t.Errorf("timestamp %q → %q, want %q", d.timestamp, d.path, want[d.timestamp])
		}
	}
}

// TestPickDumps_UsesSharedSuffixList regresses B5: adding a suffix in
// internal/labels must be enough for the CLI to see those dumps.
func TestPickDumps_UsesSharedSuffixList(t *testing.T) {
	for _, sfx := range labels.DumpSuffixes() {
		got := pickDumps([]storage.Object{obj("t/dump-20260101T020000Z" + sfx)})
		if len(got) != 1 || got[0].timestamp != "20260101T020000Z" {
			t.Errorf("suffix %s from labels.DumpSuffixes not recognised: %+v", sfx, got)
		}
	}
}

// --- B3 / D1: detectCompression ---

func TestDetectCompression_SuffixBeatsMeta(t *testing.T) {
	log := logr.Discard()
	// zstd suffix wins even when a (stale/copied) meta claims gzip.
	if got := detectCompression("t/dump-x.sql.zst.age", &meta.MetaFile{Compression: "gzip"}, log); got != labels.CompressionZstd {
		t.Errorf("zstd suffix must win over meta: got %q", got)
	}
	// gzip suffix wins over meta claiming zstd.
	if got := detectCompression("t/dump-x.sql.gz.age", &meta.MetaFile{Compression: "zstd"}, log); got != labels.CompressionGzip {
		t.Errorf("gzip suffix must win over meta: got %q", got)
	}
	// No sidecar at all: suffix alone decides (the DR case where meta is lost).
	if got := detectCompression("t/dump-x.sql.zst.age", nil, log); got != labels.CompressionZstd {
		t.Errorf("suffix-only zstd detection: got %q", got)
	}
	// Unknown suffix: meta decides.
	if got := detectCompression("t/dump-x.age", &meta.MetaFile{Compression: "zstd"}, log); got != labels.CompressionZstd {
		t.Errorf("unknown suffix must fall back to meta: got %q", got)
	}
	// Unknown suffix and no meta: gzip (backward compatible default).
	if got := detectCompression("t/dump-x.age", nil, log); got != labels.CompressionGzip {
		t.Errorf("no information must default to gzip: got %q", got)
	}
}

func TestLoadMeta(t *testing.T) {
	st := &fakeGetStorage{bodies: map[string]string{
		"t/dump-a.meta.json": `{"target":"t","sha256":"abc","compression":"zstd"}`,
		"t/dump-b.meta.json": `not json`,
	}}
	log := logr.Discard()
	if m := loadMeta(context.Background(), st, "t/dump-a.sql.zst.age", log); m == nil || m.SHA256 != "abc" {
		t.Errorf("sidecar not loaded: %+v", m)
	}
	if m := loadMeta(context.Background(), st, "t/dump-b.sql.gz.age", log); m != nil {
		t.Error("unparseable sidecar must yield nil, not a half-filled meta")
	}
	if m := loadMeta(context.Background(), st, "t/dump-c.sql.gz.age", log); m != nil {
		t.Error("missing sidecar must yield nil")
	}
	if m := loadMeta(context.Background(), st, "t/dump-a.weird", log); m != nil {
		t.Error("unknown dump suffix cannot derive a sidecar path")
	}
}

// --- D6: extractTimestamp ---

func TestExtractTimestamp_EdgeCases(t *testing.T) {
	want := time.Date(2026, 4, 29, 2, 0, 0, 0, time.UTC)
	cases := map[string]bool{
		"t/2026/04/29/dump-20260429T020000Z.sql.gz.age":  true,
		"t/2026/04/29/dump-20260429T020000Z.sql.zst.age": true,
		"t/2026/04/29/dump-20260429T020000Z.meta.json":   true,
		"dump-20260429T020000Z.sql.gz.age":               true,  // no directory
		"t/2026/04/29/notes-20260429T020000Z.sql.gz.age": false, // wrong prefix
		"t/2026/04/29/dump-manual.sql.gz.age":            false, // not a timestamp
		"t/2026/04/29/dump-20260429T020000.sql.gz.age":   false, // missing Z
		"t/2026/04/29/dump-20260431T020000Z.sql.gz.age":  false, // April 31st
		"t/2026/04/29/dump-.sql.gz.age":                  false,
		"":                                               false,
	}
	for p, ok := range cases {
		got := extractTimestamp(p)
		if ok && !got.Equal(want) {
			t.Errorf("extractTimestamp(%q) = %v, want %v", p, got, want)
		}
		if !ok && !got.IsZero() {
			t.Errorf("extractTimestamp(%q) = %v, want zero", p, got)
		}
	}
}

// --- B4: merging across destinations ---

func TestMergeDumps_DedupesAndRecordsDestinations(t *testing.T) {
	listings := map[string][]storage.Object{
		"a": {obj("t/dump-20260101T020000Z.sql.gz.age"), obj("t/dump-20260102T020000Z.sql.gz.age")},
		"b": {obj("t/dump-20260102T020000Z.sql.gz.age"), obj("t/dump-20260103T020000Z.sql.gz.age")},
		"c": nil, // listed fine, holds nothing
	}
	dumps := mergeDumps([]string{"a", "b", "c"}, listings)
	if len(dumps) != 3 {
		t.Fatalf("expected 3 distinct runs, got %d", len(dumps))
	}
	if dumps[0].timestamp != "20260103T020000Z" || strings.Join(dumps[0].dests, ",") != "b" {
		t.Errorf("newest run must come first with its holder: %+v", dumps[0])
	}
	if dumps[1].timestamp != "20260102T020000Z" || strings.Join(dumps[1].dests, ",") != "a,b" {
		t.Errorf("shared run must list both destinations in --destination order: %+v", dumps[1])
	}
	if got := newestPerDest(dumps, []string{"a", "b", "c"}); got != "a=20260102T020000Z b=20260103T020000Z c=none" {
		t.Errorf("newestPerDest = %q", got)
	}
}

func TestOpenDump_FallsThroughToNextDestination(t *testing.T) {
	good := &fakeGetStorage{bodies: map[string]string{"t/dump-x.sql.gz.age": "cipher"}}
	bad := &fakeGetStorage{bodies: map[string]string{}}
	stores := []destStorage{{name: "bad", st: bad}, {name: "good", st: good}}
	pick := dumpEntry{timestamp: "x", path: "t/dump-x.sql.gz.age", dests: []string{"bad", "good"}}
	od, err := openDump(context.Background(), stores, pick)
	if err != nil {
		t.Fatalf("openDump: %v", err)
	}
	defer func() { _ = od.rc.Close() }()
	if od.name != "good" {
		t.Errorf("expected fall-through to 'good', got %q", od.name)
	}
	body, _ := io.ReadAll(od.rc)
	if string(body) != "cipher" {
		t.Errorf("body = %q", body)
	}
	pick.dests = []string{"bad"}
	if _, err := openDump(context.Background(), stores, pick); err == nil {
		t.Error("no destination can serve → error")
	}
}

// --- B6: SHA256 verification ---

func TestVerifySHA256(t *testing.T) {
	data := []byte("encrypted bytes")
	sum := sha256.Sum256(data)
	h := sha256.New()
	_, _ = io.Copy(h, bytes.NewReader(data))
	if err := verifySHA256(hex.EncodeToString(sum[:]), h); err != nil {
		t.Errorf("matching digest rejected: %v", err)
	}
	if err := verifySHA256(strings.ToUpper(hex.EncodeToString(sum[:])), h); err != nil {
		t.Errorf("digest comparison must be case-insensitive: %v", err)
	}
	if err := verifySHA256("deadbeef", h); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("wrong digest must fail loudly, got %v", err)
	}
}

func TestDestList_Set(t *testing.T) {
	var d destList
	_ = d.Set("a, b")
	_ = d.Set("c")
	_ = d.Set(" ,")
	if d.String() != "a,b,c" {
		t.Errorf("destList = %q", d.String())
	}
}

func TestDecryptHint(t *testing.T) {
	if h := decryptHint(errors.New("something else")); h != "" {
		t.Errorf("unrelated error must get no hint, got %q", h)
	}
	if h := decryptHint(errors.New("failed to read header")); !strings.Contains(h, "age header") {
		t.Errorf("header error must hint at a non-age object, got %q", h)
	}
}
