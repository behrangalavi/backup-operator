package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"backup-operator/internal/secrets"
	"backup-operator/storage"

	"github.com/go-logr/logr"
)

// --- dumpPathFromMeta tests ---

func TestDumpPathFromMeta(t *testing.T) {
	cases := []struct {
		name        string
		metaPath    string
		compression string
		want        string
	}{
		{"gzip", "prod/2026/06/03/dump-x.meta.json", "gzip", "prod/2026/06/03/dump-x.sql.gz.age"},
		{"empty compression defaults to gzip", "prod/dump-x.meta.json", "", "prod/dump-x.sql.gz.age"},
		{"zstd", "prod/dump-x.meta.json", "zstd", "prod/dump-x.sql.zst.age"},
		{"not a meta path", "prod/dump-x.sql.gz.age", "gzip", ""},
		{"bare name without suffix", "random", "gzip", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dumpPathFromMeta(c.metaPath, c.compression); got != c.want {
				t.Errorf("dumpPathFromMeta(%q, %q) = %q, want %q", c.metaPath, c.compression, got, c.want)
			}
		})
	}
}

// --- scrubOne tests ---

// fakeScrubStorage serves a fixed set of objects for List and bytes for Get,
// recording which paths were fetched so a test can prove which scrub branch
// ran without inspecting global Prometheus state. It deliberately does NOT
// implement storage.BatchStorage, so loadLatestMeta uses it directly.
type fakeScrubStorage struct {
	name     string
	objects  []storage.Object
	contents map[string][]byte

	mu   sync.Mutex
	gets map[string]int
}

func (f *fakeScrubStorage) Name() string { return f.name }

func (f *fakeScrubStorage) Upload(context.Context, string, io.Reader) error { return nil }

func (f *fakeScrubStorage) List(_ context.Context, prefix string) ([]storage.Object, error) {
	var out []storage.Object
	for _, o := range f.objects {
		if strings.HasPrefix(o.Path, prefix) {
			out = append(out, o)
		}
	}
	return out, nil
}

func (f *fakeScrubStorage) Get(_ context.Context, p string) (io.ReadCloser, error) {
	f.mu.Lock()
	if f.gets == nil {
		f.gets = make(map[string]int)
	}
	f.gets[p]++
	f.mu.Unlock()
	b, ok := f.contents[p]
	if !ok {
		return nil, fmt.Errorf("not found: %s", p)
	}
	return io.NopCloser(strings.NewReader(string(b))), nil
}

func (f *fakeScrubStorage) Delete(context.Context, string) error { return nil }

func (f *fakeScrubStorage) getCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets[path]
}

// scrubDest is the destination injected into the pool.
func scrubDest() *secrets.Destination {
	return &secrets.Destination{Name: "hetzner", StorageType: "sftp"}
}

// injectFakeStorage pre-populates the pool so Pool.Get(d) returns the fake
// rather than constructing a real backend via the factory.
func injectFakeStorage(p *StoragePool, d *secrets.Destination, st storage.Storage) {
	p.mu.Lock()
	p.clients[d.Name] = pooledClient{sig: destSignature(d), storage: st}
	p.mu.Unlock()
}

// scrubFixture builds a fake holding one meta + matching dump for "prod".
// metaJSON is the literal sidecar; dump is the encrypted-dump bytes whose
// real sha256 is returned so the caller can craft matching/mismatching metas.
func scrubFixture(metaJSON, dump string) (*fakeScrubStorage, string, string, string) {
	metaPath := "prod/2026/06/03/dump-20260603T000000Z.meta.json"
	dumpPath := "prod/2026/06/03/dump-20260603T000000Z.sql.gz.age"
	sum := sha256.Sum256([]byte(dump))
	st := &fakeScrubStorage{
		name: "hetzner",
		objects: []storage.Object{
			// Non-zero LastModified so mostRecentMeta selects this meta.
			{Path: metaPath, Size: int64(len(metaJSON)), LastModified: time.Unix(1_700_000_000, 0)},
			{Path: dumpPath, Size: int64(len(dump))},
		},
		contents: map[string][]byte{
			metaPath: []byte(metaJSON),
			dumpPath: []byte(dump),
		},
	}
	return st, metaPath, dumpPath, hex.EncodeToString(sum[:])
}

func newScrubber(p *StoragePool) *StorageScrubber {
	return &StorageScrubber{Logger: logr.Discard(), Namespace: "backup", Pool: p}
}

func TestScrubOne_HappyPathFetchesDump(t *testing.T) {
	dump := "encrypted-dump-bytes"
	st, _, dumpPath, sum := scrubFixture("", dump)
	st.contents["prod/2026/06/03/dump-20260603T000000Z.meta.json"] =
		[]byte(fmt.Sprintf(`{"target":"prod","status":"success","sha256":%q,"encryptedSizeBytes":%d}`, sum, len(dump)))

	p := NewStoragePool(logr.Discard())
	d := scrubDest()
	injectFakeStorage(p, d, st)

	newScrubber(p).scrubOne(context.Background(), "prod", d)

	if st.getCount(dumpPath) != 1 {
		t.Errorf("happy path should fetch the dump exactly once, got %d", st.getCount(dumpPath))
	}
}

func TestScrubOne_SizeMismatchShortCircuitsBeforeGet(t *testing.T) {
	dump := "encrypted-dump-bytes"
	st, _, dumpPath, sum := scrubFixture("", dump)
	// Meta claims a different size than the listed object → pre-check fails.
	st.contents["prod/2026/06/03/dump-20260603T000000Z.meta.json"] =
		[]byte(fmt.Sprintf(`{"target":"prod","status":"success","sha256":%q,"encryptedSizeBytes":%d}`, sum, len(dump)+999))

	p := NewStoragePool(logr.Discard())
	d := scrubDest()
	injectFakeStorage(p, d, st)

	newScrubber(p).scrubOne(context.Background(), "prod", d)

	if st.getCount(dumpPath) != 0 {
		t.Errorf("size pre-check mismatch must short-circuit before fetching the dump, got %d fetches", st.getCount(dumpPath))
	}
}

func TestScrubOne_FailureMetaSkipped(t *testing.T) {
	st, _, dumpPath, _ := scrubFixture("", "x")
	st.contents["prod/2026/06/03/dump-20260603T000000Z.meta.json"] =
		[]byte(`{"target":"prod","status":"failed"}`)

	p := NewStoragePool(logr.Discard())
	d := scrubDest()
	injectFakeStorage(p, d, st)

	newScrubber(p).scrubOne(context.Background(), "prod", d)

	if st.getCount(dumpPath) != 0 {
		t.Errorf("failure-meta has no dump; must not fetch, got %d", st.getCount(dumpPath))
	}
}

func TestScrubOne_LegacyMetaWithoutSHASkipped(t *testing.T) {
	st, _, dumpPath, _ := scrubFixture("", "x")
	st.contents["prod/2026/06/03/dump-20260603T000000Z.meta.json"] =
		[]byte(`{"target":"prod","status":"success"}`) // no sha256

	p := NewStoragePool(logr.Discard())
	d := scrubDest()
	injectFakeStorage(p, d, st)

	newScrubber(p).scrubOne(context.Background(), "prod", d)

	if st.getCount(dumpPath) != 0 {
		t.Errorf("legacy meta without sha256 cannot be verified; must not fetch, got %d", st.getCount(dumpPath))
	}
}

func TestScrubOne_NoMetaIsNoop(t *testing.T) {
	st := &fakeScrubStorage{name: "hetzner", contents: map[string][]byte{}}
	p := NewStoragePool(logr.Discard())
	d := scrubDest()
	injectFakeStorage(p, d, st)

	// No objects listed → loadLatestMeta returns not-found; must not panic.
	newScrubber(p).scrubOne(context.Background(), "prod", d)
}
