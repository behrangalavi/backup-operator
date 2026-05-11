package ftps

import (
	"strings"
	"testing"

	"backup-operator/storage"

	"github.com/go-logr/logr"
)

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
