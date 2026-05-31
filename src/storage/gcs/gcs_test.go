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
