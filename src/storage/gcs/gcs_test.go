package gcs

import (
	"testing"

	"backup-operator/storage"

	"github.com/go-logr/logr"
)

func TestNew_MissingBucket(t *testing.T) {
	_, err := New("test", storage.SecretData{
		keySAJSON: []byte(`{"type":"service_account"}`),
	}, logr.Discard())
	if err == nil {
		t.Fatal("expected error for missing bucket")
	}
}

func TestNew_MissingServiceAccountJSON(t *testing.T) {
	_, err := New("test", storage.SecretData{
		keyBucket: []byte("my-bucket"),
	}, logr.Discard())
	if err == nil {
		t.Fatal("expected error for missing service account JSON")
	}
}

func TestNew_InvalidJSON(t *testing.T) {
	_, err := New("test", storage.SecretData{
		keyBucket: []byte("my-bucket"),
		keySAJSON: []byte("not json at all"),
	}, logr.Discard())
	if err == nil {
		t.Fatal("expected error for invalid service account JSON")
	}
}

func TestFull_NoPrefix(t *testing.T) {
	g := &gcsStorage{pathPrefix: ""}
	cases := []struct{ in, want string }{
		{"backups/dump.sql", "backups/dump.sql"},
		{"/backups/dump.sql", "backups/dump.sql"},
	}
	for _, c := range cases {
		if got := g.full(c.in); got != c.want {
			t.Errorf("full(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFull_WithPrefix(t *testing.T) {
	g := &gcsStorage{pathPrefix: "cluster-prod"}
	cases := []struct{ in, want string }{
		{"target/dump.sql", "cluster-prod/target/dump.sql"},
		{"/target/dump.sql", "cluster-prod/target/dump.sql"},
	}
	for _, c := range cases {
		if got := g.full(c.in); got != c.want {
			t.Errorf("full(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStripPrefix_NoPrefix(t *testing.T) {
	g := &gcsStorage{pathPrefix: ""}
	if got := g.stripPrefix("backups/dump.sql"); got != "backups/dump.sql" {
		t.Errorf("stripPrefix() = %q, want %q", got, "backups/dump.sql")
	}
}

func TestStripPrefix_WithPrefix(t *testing.T) {
	g := &gcsStorage{pathPrefix: "cluster-prod"}
	cases := []struct{ in, want string }{
		{"cluster-prod/target/dump.sql", "target/dump.sql"},
		{"cluster-prod/target/meta.json", "target/meta.json"},
	}
	for _, c := range cases {
		if got := g.stripPrefix(c.in); got != c.want {
			t.Errorf("stripPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestFull_ListPrefixKeepsTrailingSlash is the regression guard for the
// prefix-bleed bug: a List prefix of "db/" must stay "prefix/db/" and not
// collapse to "prefix/db", which would also match the sibling target
// "prefix/db-archive/..." and let one target's retention delete another's.
func TestFull_ListPrefixKeepsTrailingSlash(t *testing.T) {
	s := &gcsStorage{pathPrefix: "cluster-prod"}
	got := s.full("db/")
	if got != "cluster-prod/db/" {
		t.Fatalf("full(%q) = %q, want %q (trailing slash must survive)", "db/", got, "cluster-prod/db/")
	}
	siblingKey := "cluster-prod/db-archive/2026/01/01/dump-x.sql.gz.age"
	if len(siblingKey) >= len(got) && siblingKey[:len(got)] == got {
		t.Fatalf("List prefix %q still matches sibling target key %q", got, siblingKey)
	}
}

// TestFull_RootListKeepsSeparator guards the empty-prefix (root) listing used
// by LatestPerTarget: "prefix/" must not match a sibling destination prefix
// like "prefix-eu/...", and stripPrefix must only strip the exact boundary.
func TestFull_RootListKeepsSeparator(t *testing.T) {
	s := &gcsStorage{pathPrefix: "prod"}
	if got := s.full(""); got != "prod/" {
		t.Fatalf("full(%q) = %q, want %q", "", got, "prod/")
	}
	if got := s.stripPrefix("prod-eu/db/x"); got != "prod-eu/db/x" {
		t.Fatalf("stripPrefix must only strip the exact %q boundary, got %q", "prod/", got)
	}
}
