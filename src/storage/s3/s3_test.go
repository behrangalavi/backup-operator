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

// TestFull_ListPrefixKeepsTrailingSlash is the regression guard for the
// prefix-bleed bug: a List prefix of "db/" must stay "prefix/db/" and not
// collapse to "prefix/db", which would also match the sibling target
// "prefix/db-archive/..." and let one target's retention delete another's.
func TestFull_ListPrefixKeepsTrailingSlash(t *testing.T) {
	s := &s3Storage{pathPrefix: "cluster-prod"}
	got := s.full("db/")
	if got != "cluster-prod/db/" {
		t.Fatalf("full(%q) = %q, want %q (trailing slash must survive)", "db/", got, "cluster-prod/db/")
	}
	// The bleed check: the produced List prefix must NOT be a string prefix of
	// a sibling target's key.
	siblingKey := "cluster-prod/db-archive/2026/01/01/dump-x.sql.gz.age"
	if len(siblingKey) >= len(got) && siblingKey[:len(got)] == got {
		t.Fatalf("List prefix %q still matches sibling target key %q", got, siblingKey)
	}
}

// TestFull_RootListKeepsSeparator guards the empty-prefix (root) listing used
// by LatestPerTarget: "prefix/" must not match a sibling destination prefix
// like "prefix-eu/...".
func TestFull_RootListKeepsSeparator(t *testing.T) {
	s := &s3Storage{pathPrefix: "prod"}
	if got := s.full(""); got != "prod/" {
		t.Fatalf("full(%q) = %q, want %q", "", got, "prod/")
	}
	// stripPrefix must not mangle a sibling key it should never see, but if it
	// does encounter "prod-eu/..." it must not turn it into "-eu/...".
	if got := s.stripPrefix("prod-eu/db/x"); got != "prod-eu/db/x" {
		t.Fatalf("stripPrefix must only strip the exact %q boundary, got %q", "prod/", got)
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
