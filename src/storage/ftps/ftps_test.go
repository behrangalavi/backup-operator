package ftps

import (
	"errors"
	"path"
	"strings"
	"testing"

	"backup-operator/storage"

	"github.com/go-logr/logr"
)

var errFake550 = errors.New("550 fake error")

// New only validates config and prepares the dial spec; it does not open a
// connection. Tests assert that the validation logic correctly accepts or
// rejects inputs without needing a live FTP server.

func TestNew_MinimalConfig(t *testing.T) {
	s, err := New("test", storage.SecretData{
		keyHost:     []byte("ftp.example.com"),
		keyUsername: []byte("user"),
		keyPassword: []byte("pw"),
	}, logr.Discard())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Name() != "test" {
		t.Errorf("expected Name()=test, got %q", s.Name())
	}
}

func TestNew_MissingHost(t *testing.T) {
	_, err := New("test", storage.SecretData{
		keyUsername: []byte("user"),
		keyPassword: []byte("pw"),
	}, logr.Discard())
	if err == nil || !strings.Contains(err.Error(), "host") {
		t.Fatalf("expected missing host error, got: %v", err)
	}
}

func TestNew_MissingPassword(t *testing.T) {
	_, err := New("test", storage.SecretData{
		keyHost:     []byte("ftp.example.com"),
		keyUsername: []byte("user"),
	}, logr.Discard())
	if err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("expected missing password error, got: %v", err)
	}
}

func TestNew_DefaultPortExplicit(t *testing.T) {
	s, err := New("test", storage.SecretData{
		keyHost:     []byte("ftp.example.com"),
		keyUsername: []byte("user"),
		keyPassword: []byte("pw"),
	}, logr.Discard())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fs := s.(*ftpsStorage)
	if !strings.HasSuffix(fs.addr, ":21") {
		t.Errorf("expected default port 21 for explicit mode, got addr=%q", fs.addr)
	}
	if fs.implicitTLS {
		t.Errorf("expected explicit mode by default")
	}
}

func TestNew_DefaultPortImplicit(t *testing.T) {
	s, err := New("test", storage.SecretData{
		keyHost:     []byte("ftp.example.com"),
		keyUsername: []byte("user"),
		keyPassword: []byte("pw"),
		keyTLSMode:  []byte("implicit"),
	}, logr.Discard())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fs := s.(*ftpsStorage)
	if !strings.HasSuffix(fs.addr, ":990") {
		t.Errorf("expected default port 990 for implicit mode, got addr=%q", fs.addr)
	}
	if !fs.implicitTLS {
		t.Errorf("expected implicit mode")
	}
}

func TestNew_InvalidTLSMode(t *testing.T) {
	_, err := New("test", storage.SecretData{
		keyHost:     []byte("ftp.example.com"),
		keyUsername: []byte("user"),
		keyPassword: []byte("pw"),
		keyTLSMode:  []byte("plaintext"), // not allowed
	}, logr.Discard())
	if err == nil || !strings.Contains(err.Error(), "tls-mode") {
		t.Fatalf("expected invalid tls-mode error, got: %v", err)
	}
}

func TestNew_InvalidPort(t *testing.T) {
	_, err := New("test", storage.SecretData{
		keyHost:     []byte("ftp.example.com"),
		keyUsername: []byte("user"),
		keyPassword: []byte("pw"),
		keyPort:     []byte("not-a-number"),
	}, logr.Discard())
	if err == nil || !strings.Contains(err.Error(), "port") {
		t.Fatalf("expected invalid port error, got: %v", err)
	}
}

// The destination's path-prefix must round-trip through full→stripPrefix so
// retention can pass List results back to Delete without double-prefixing —
// the same invariant the SFTP and S3 drivers guarantee.
func TestStripPrefixRoundtrip(t *testing.T) {
	s, err := New("test", storage.SecretData{
		keyHost:       []byte("ftp.example.com"),
		keyUsername:   []byte("user"),
		keyPassword:   []byte("pw"),
		keyPathPrefix: []byte("/cluster-prod"),
	}, logr.Discard())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fs := s.(*ftpsStorage)
	logical := "target/2026/05/11/dump-x.sql.gz.age"
	full := fs.full(logical)
	got := fs.stripPrefix(full)
	if got != logical {
		t.Errorf("roundtrip mismatch: logical=%q full=%q stripped=%q", logical, full, got)
	}
}

func TestStripPrefix_NoPrefix(t *testing.T) {
	s, err := New("test", storage.SecretData{
		keyHost:     []byte("ftp.example.com"),
		keyUsername: []byte("user"),
		keyPassword: []byte("pw"),
	}, logr.Discard())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fs := s.(*ftpsStorage)
	if fs.full("a/b") != "a/b" {
		t.Errorf("no-prefix full() should be identity")
	}
	if fs.stripPrefix("a/b") != "a/b" {
		t.Errorf("no-prefix stripPrefix() should be identity")
	}
}

// --- mkdirAll CWD-restore regression ---

// fakeFTP models an FTP server's namespace + working directory so mkdirAll's
// CWD handling can be unit-tested without a live server. Directory existence
// is tracked as a set of absolute paths; MakeDir/ChangeDir resolve relative
// paths against the current cwd exactly as a real server does.
type fakeFTP struct {
	cwd     string
	exists  map[string]bool
	noPWD   bool // simulate a server without PWD support
	makeErr error
}

func newFakeFTP(login string, dirs ...string) *fakeFTP {
	f := &fakeFTP{cwd: login, exists: map[string]bool{login: true}}
	for _, d := range dirs {
		f.exists[d] = true
	}
	return f
}

func (f *fakeFTP) resolve(p string) string {
	if strings.HasPrefix(p, "/") {
		return path.Clean(p)
	}
	return path.Clean(f.cwd + "/" + p)
}

func (f *fakeFTP) MakeDir(p string) error {
	if f.makeErr != nil {
		return f.makeErr
	}
	abs := f.resolve(p)
	if f.exists[abs] {
		return errFake550 // already exists
	}
	if !f.exists[path.Dir(abs)] {
		return errFake550 // missing parent
	}
	f.exists[abs] = true
	return nil
}

func (f *fakeFTP) ChangeDir(p string) error {
	abs := f.resolve(p)
	if !f.exists[abs] {
		return errFake550
	}
	f.cwd = abs
	return nil
}

func (f *fakeFTP) CurrentDir() (string, error) {
	if f.noPWD {
		return "", errFake550
	}
	return f.cwd, nil
}

// TestMkdirAll_RestoresCWD is the regression guard: when a leading segment
// already exists (relative prefix), the existence probe must not leave the
// CWD inside the tree — otherwise the next MakeDir doubles the prefix and the
// whole create fails. Asserts success, CWD back at login, and the full tree
// created under the login dir (not a doubled path).
func TestMkdirAll_RestoresCWD(t *testing.T) {
	login := "/home/user"
	// "t" already exists from a prior upload; the deeper partitions do not.
	f := newFakeFTP(login, login+"/t")

	if err := mkdirAll(f, "t/2026/07/29"); err != nil {
		t.Fatalf("mkdirAll: %v", err)
	}
	if f.cwd != login {
		t.Errorf("CWD not restored: got %q, want %q", f.cwd, login)
	}
	for _, want := range []string{
		"/home/user/t/2026", "/home/user/t/2026/07", "/home/user/t/2026/07/29",
	} {
		if !f.exists[want] {
			t.Errorf("expected %q to be created under the login dir", want)
		}
	}
	// The doubled-prefix path the bug produced must NOT exist.
	if f.exists["/home/user/t/t"] {
		t.Error("doubled-prefix directory /home/user/t/t was created — CWD bug present")
	}
}

// TestMkdirAll_NoPWDSkipsProbe: a server without PWD support can't be safely
// probed, so mkdirAll treats a MakeDir 550 as "probably exists" and returns
// nil rather than corrupting the session.
func TestMkdirAll_NoPWDSkipsProbe(t *testing.T) {
	login := "/home/user"
	f := newFakeFTP(login, login+"/t")
	f.noPWD = true
	if err := mkdirAll(f, "t/2026"); err != nil {
		t.Fatalf("mkdirAll with no PWD should not error on existing dir: %v", err)
	}
	if f.cwd != login {
		t.Errorf("CWD must be untouched when probe is skipped, got %q", f.cwd)
	}
}

// TestMkdirAll_RealFailureSurfaces: when a directory genuinely can't be
// created or entered (missing parent, no permission), the error propagates.
func TestMkdirAll_RealFailureSurfaces(t *testing.T) {
	f := newFakeFTP("/home/user")
	// makeErr forces every MakeDir to fail; the dir doesn't exist so the
	// ChangeDir probe also fails → error must surface.
	f.makeErr = errFake550
	if err := mkdirAll(f, "nope"); err == nil {
		t.Error("expected mkdirAll to surface a genuine create/probe failure")
	}
}
