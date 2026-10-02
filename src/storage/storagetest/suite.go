package storagetest

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"backup-operator/storage"
)

// Run executes the Storage conformance suite against a fresh instance from
// newFn for every subtest. The instance must start EMPTY under the prefix it
// is configured with; the suite uploads, lists, reads and deletes its own
// objects. Backends with a real network dependency run it from an
// integration test behind a build tag; the in-memory fake runs it always.
func Run(t *testing.T, newFn func(t *testing.T) storage.Storage) {
	t.Helper()
	ctx := context.Background()
	cases := []struct {
		name string
		fn   func(t *testing.T, ctx context.Context, newFn func(t *testing.T) storage.Storage)
	}{
		{"upload-get-roundtrip", testUploadGetRoundtrip},
		{"list-returns-logical-paths-that-roundtrip", testListReturnsLogicalPathsThatRoundtrip},
		{"list-prefix-does-not-bleed-into-sibling-target", testListPrefixDoesNotBleedIntoSiblingTarget},
		{"get-missing-wraps-ErrNotFound", testGetMissingWrapsErrNotFound},
		{"delete-missing-is-idempotent", testDeleteMissingIsIdempotent},
		{"delete-removes-object", testDeleteRemovesObject},
		{"list-empty-prefix-is-not-an-error", testListEmptyPrefixIsNotAnError},
		{"cancelled-context-is-honoured", testCancelledContextIsHonoured},
		{"batch-session-shares-semantics", testBatchSessionSharesSemantics},
		{"close-is-safe", testCloseIsSafe},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) { c.fn(t, ctx, newFn) })
	}
}

func testUploadGetRoundtrip(t *testing.T, ctx context.Context, newFn func(t *testing.T) storage.Storage) {
	st := newFn(t)
	defer func() { _ = st.Close() }()
	want := []byte("ciphertext-bytes")
	if err := st.Upload(ctx, "db/2026/01/01/dump-a.sql.gz.age", strings.NewReader(string(want))); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	rc, err := st.Get(ctx, "db/2026/01/01/dump-a.sql.gz.age")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(got) != string(want) {
		t.Fatalf("Get returned %q (%v), want %q", got, err, want)
	}
}

func testListReturnsLogicalPathsThatRoundtrip(t *testing.T, ctx context.Context, newFn func(t *testing.T) storage.Storage) {
	st := newFn(t)
	defer func() { _ = st.Close() }()
	paths := []string{"db/2026/01/01/dump-a.sql.gz.age", "db/2026/01/01/dump-a.meta.json"}
	for _, p := range paths {
		if err := st.Upload(ctx, p, strings.NewReader("x")); err != nil {
			t.Fatalf("Upload %s: %v", p, err)
		}
	}
	objs, err := st.List(ctx, "db/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != len(paths) {
		t.Fatalf("List returned %d objects, want %d: %+v", len(objs), len(paths), objs)
	}
	for _, o := range objs {
		if strings.HasPrefix(o.Path, "/") {
			t.Errorf("List path %q must be relative", o.Path)
		}
		if o.Size != 1 {
			t.Errorf("List size for %q = %d, want 1", o.Path, o.Size)
		}
		// The listed path must be usable verbatim with Get — that is the
		// round-trip the analyzer, retention and the UI rely on.
		rc, err := st.Get(ctx, o.Path)
		if err != nil {
			t.Errorf("Get(List path %q): %v — List must return prefix-stripped logical paths", o.Path, err)
			continue
		}
		_ = rc.Close()
	}
}

func testListPrefixDoesNotBleedIntoSiblingTarget(t *testing.T, ctx context.Context, newFn func(t *testing.T) storage.Storage) {
	st := newFn(t)
	defer func() { _ = st.Close() }()
	for _, p := range []string{"db/2026/01/01/dump-a.sql.gz.age", "db-archive/2026/01/01/dump-b.sql.gz.age"} {
		if err := st.Upload(ctx, p, strings.NewReader("x")); err != nil {
			t.Fatalf("Upload %s: %v", p, err)
		}
	}
	objs, err := st.List(ctx, "db/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, o := range objs {
		if strings.HasPrefix(o.Path, "db-archive/") {
			t.Fatalf("List(%q) returned sibling target object %q — retention for one target would delete another's dumps", "db/", o.Path)
		}
	}
	if len(objs) != 1 {
		t.Fatalf("List(%q) = %+v, want exactly the one db/ object", "db/", objs)
	}
}

func testGetMissingWrapsErrNotFound(t *testing.T, ctx context.Context, newFn func(t *testing.T) storage.Storage) {
	st := newFn(t)
	defer func() { _ = st.Close() }()
	rc, err := st.Get(ctx, "db/does/not/exist.age")
	if err == nil {
		_ = rc.Close()
		t.Fatal("Get of a missing object must fail")
	}
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Get of a missing object must wrap storage.ErrNotFound, got %v", err)
	}
}

func testDeleteMissingIsIdempotent(t *testing.T, ctx context.Context, newFn func(t *testing.T) storage.Storage) {
	st := newFn(t)
	defer func() { _ = st.Close() }()
	if err := st.Delete(ctx, "db/never/uploaded.age"); err != nil {
		t.Fatalf("Delete of a missing object must succeed (idempotent), got %v", err)
	}
}

func testDeleteRemovesObject(t *testing.T, ctx context.Context, newFn func(t *testing.T) storage.Storage) {
	st := newFn(t)
	defer func() { _ = st.Close() }()
	if err := st.Upload(ctx, "db/x.age", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "db/x.age"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Get(ctx, "db/x.age"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("object must be gone after Delete, Get returned %v", err)
	}
	objs, err := st.List(ctx, "db/")
	if err != nil || len(objs) != 0 {
		t.Fatalf("List after Delete = %+v (%v), want empty", objs, err)
	}
	// Second delete converges.
	if err := st.Delete(ctx, "db/x.age"); err != nil {
		t.Fatalf("repeated Delete must succeed, got %v", err)
	}
}

func testListEmptyPrefixIsNotAnError(t *testing.T, ctx context.Context, newFn func(t *testing.T) storage.Storage) {
	st := newFn(t)
	defer func() { _ = st.Close() }()
	objs, err := st.List(ctx, "fresh-target/")
	if err != nil {
		t.Fatalf("List of a never-written prefix must be an empty success (first-run case), got %v", err)
	}
	if len(objs) != 0 {
		t.Fatalf("expected empty listing, got %+v", objs)
	}
}

func testCancelledContextIsHonoured(t *testing.T, ctx context.Context, newFn func(t *testing.T) storage.Storage) {
	st := newFn(t)
	defer func() { _ = st.Close() }()
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := st.Upload(cctx, "db/x.age", strings.NewReader("x")); err == nil {
		t.Error("Upload with a cancelled context must fail")
	}
	if _, err := st.List(cctx, "db/"); err == nil {
		t.Error("List with a cancelled context must fail")
	}
}

func testBatchSessionSharesSemantics(t *testing.T, ctx context.Context, newFn func(t *testing.T) storage.Storage) {
	st := newFn(t)
	defer func() { _ = st.Close() }()
	bs, ok := st.(storage.BatchStorage)
	if !ok {
		t.Skip("backend has no BatchStorage")
	}
	sess, closer, err := bs.WithSession(ctx)
	if err != nil {
		t.Fatalf("WithSession: %v", err)
	}
	defer func() { _ = closer() }()
	if err := sess.Upload(ctx, "db/s.age", strings.NewReader("y")); err != nil {
		t.Fatalf("session Upload: %v", err)
	}
	objs, err := sess.List(ctx, "db/")
	if err != nil || len(objs) != 1 || objs[0].Path != "db/s.age" {
		t.Fatalf("session List = %+v (%v)", objs, err)
	}
	if err := sess.Delete(ctx, "db/s.age"); err != nil {
		t.Fatalf("session Delete: %v", err)
	}
	if err := sess.Delete(ctx, "db/s.age"); err != nil {
		t.Fatalf("session Delete of a missing object must be idempotent, got %v", err)
	}
	if _, err := sess.Get(ctx, "db/s.age"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("session Get of a missing object must wrap ErrNotFound, got %v", err)
	}
}

func testCloseIsSafe(t *testing.T, ctx context.Context, newFn func(t *testing.T) storage.Storage) {
	st := newFn(t)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
