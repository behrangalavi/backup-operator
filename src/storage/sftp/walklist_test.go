package sftp

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"
)

// newPipeClient wires an in-memory pkg/sftp server (serving the local OS
// filesystem) to a client over net.Pipe, so walkList can be exercised against
// real absolute paths without a network or SSH layer. The server serves the
// whole local FS; tests pass absolute paths under t.TempDir().
func newPipeClient(t *testing.T) *sftp.Client {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	srv, err := sftp.NewServer(serverConn)
	if err != nil {
		t.Fatalf("new sftp server: %v", err)
	}
	go func() { _ = srv.Serve() }()
	client, err := sftp.NewClientPipe(clientConn, clientConn)
	if err != nil {
		t.Fatalf("new sftp client pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = srv.Close()
	})
	return client
}

// TestWalkList_MissingRootReturnsEmpty regresses the bug where List against a
// not-yet-created target directory returned an error (unlike S3/Azure/GCS and
// FTPS), causing false destination_failed / baseline-unavailable / retention
// signals on every brand-new SFTP target's first run.
func TestWalkList_MissingRootReturnsEmpty(t *testing.T) {
	client := newPipeClient(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist-yet")

	out, err := walkList(context.Background(), client, missing, func(s string) string { return s })
	if err != nil {
		t.Fatalf("missing root must not error, got: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("missing root must list empty, got %d objects", len(out))
	}
}

// TestWalkList_ExistingTreeListsFiles confirms the happy path still returns the
// non-directory entries after the missing-root special case was added.
func TestWalkList_ExistingTreeListsFiles(t *testing.T) {
	client := newPipeClient(t)
	root := t.TempDir()
	sub := filepath.Join(root, "2026", "01")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "dump-a.age"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "dump-a.meta.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, err := walkList(context.Background(), client, root, func(s string) string { return s })
	if err != nil {
		t.Fatalf("walk existing tree: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 files, got %d: %+v", len(out), out)
	}
}
