package s3

import (
	"testing"

	"backup-operator/storage"

	"github.com/go-logr/logr"
)

func TestNew_MissingBucket(t *testing.T) {
	_, err := New("test", storage.SecretData{
		keyAccessKey: []byte("AKIA..."),
		keySecretKey: []byte("secret"),
	}, logr.Discard())
	if err == nil {
		t.Fatal("expected error for missing bucket")
	}
}

func TestNew_MissingAccessKey(t *testing.T) {
	_, err := New("test", storage.SecretData{
		keyBucket:    []byte("my-bucket"),
		keySecretKey: []byte("secret"),
	}, logr.Discard())
	if err == nil {
		t.Fatal("expected error for missing access key")
	}
}

func TestNew_MissingSecretKey(t *testing.T) {
	_, err := New("test", storage.SecretData{
		keyBucket:    []byte("my-bucket"),
		keyAccessKey: []byte("AKIA..."),
	}, logr.Discard())
	if err == nil {
		t.Fatal("expected error for missing secret key")
	}
}

func TestNew_Success(t *testing.T) {
	s, err := New("hetzner-obj", storage.SecretData{
		keyBucket:    []byte("backups"),
		keyAccessKey: []byte("AKIA1234"),
		keySecretKey: []byte("secret1234"),
	}, logr.Discard())
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	if got := s.Name(); got != "hetzner-obj" {
		t.Errorf("Name() = %q, want %q", got, "hetzner-obj")
	}
}

func TestNew_TrimWhitespace(t *testing.T) {
	s, err := New("test", storage.SecretData{
		keyBucket:    []byte("  my-bucket  \n"),
		keyAccessKey: []byte("  AKIA  "),
		keySecretKey: []byte("  secret  "),
	}, logr.Discard())
	if err != nil {
		t.Fatalf("New() should trim whitespace: %v", err)
	}
	st := s.(*s3Storage)
	if st.bucket != "my-bucket" {
		t.Errorf("bucket = %q, want %q (trimmed)", st.bucket, "my-bucket")
	}
}

func TestNew_DefaultRegion(t *testing.T) {
	_, err := New("test", storage.SecretData{
		keyBucket:    []byte("b"),
		keyAccessKey: []byte("a"),
		keySecretKey: []byte("s"),
	}, logr.Discard())
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
}

func TestFull_NoPrefix(t *testing.T) {
	s := &s3Storage{pathPrefix: ""}
	cases := []struct{ in, want string }{
		{"backups/dump.sql", "backups/dump.sql"},
		{"/backups/dump.sql", "backups/dump.sql"},
	}
	for _, c := range cases {
		if got := s.full(c.in); got != c.want {
			t.Errorf("full(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFull_WithPrefix(t *testing.T) {
	s := &s3Storage{pathPrefix: "cluster-prod"}
	cases := []struct{ in, want string }{
		{"target/dump.sql", "cluster-prod/target/dump.sql"},
		{"/target/dump.sql", "cluster-prod/target/dump.sql"},
	}
	for _, c := range cases {
		if got := s.full(c.in); got != c.want {
			t.Errorf("full(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStripPrefix_NoPrefix(t *testing.T) {
	s := &s3Storage{pathPrefix: ""}
	if got := s.stripPrefix("backups/dump.sql"); got != "backups/dump.sql" {
		t.Errorf("stripPrefix() = %q, want %q", got, "backups/dump.sql")
	}
}

func TestStripPrefix_WithPrefix(t *testing.T) {
	s := &s3Storage{pathPrefix: "cluster-prod"}
	cases := []struct{ in, want string }{
		{"cluster-prod/target/dump.sql", "target/dump.sql"},
		{"cluster-prod/target/meta.json", "target/meta.json"},
	}
	for _, c := range cases {
		if got := s.stripPrefix(c.in); got != c.want {
			t.Errorf("stripPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNew_WithPathPrefix(t *testing.T) {
	s, err := New("test", storage.SecretData{
		keyBucket:    []byte("b"),
		keyAccessKey: []byte("a"),
		keySecretKey: []byte("s"),
		keyPrefix:    []byte("/cluster-prod/"),
	}, logr.Discard())
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	st := s.(*s3Storage)
	if st.pathPrefix != "/cluster-prod" {
		t.Errorf("pathPrefix = %q, want %q (trailing slash stripped)", st.pathPrefix, "/cluster-prod")
	}
}
