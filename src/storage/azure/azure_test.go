package azure

import (
	"encoding/base64"
	"testing"

	"backup-operator/storage"

	"github.com/go-logr/logr"
)

// Azure SharedKeyCredential decodes the account key as base64, so tests use
// a valid base64 string.
var validKey = base64.StdEncoding.EncodeToString([]byte("test-account-key-bytes"))

func TestNew_MissingAccountName(t *testing.T) {
	_, err := New("test", storage.SecretData{
		keyAccountKey: []byte(validKey),
		keyContainer:  []byte("backups"),
	}, logr.Discard())
	if err == nil {
		t.Fatal("expected error for missing account name")
	}
}

func TestNew_MissingAccountKey(t *testing.T) {
	_, err := New("test", storage.SecretData{
		keyAccountName: []byte("myaccount"),
		keyContainer:   []byte("backups"),
	}, logr.Discard())
	if err == nil {
		t.Fatal("expected error for missing account key")
	}
}

func TestNew_MissingContainer(t *testing.T) {
	_, err := New("test", storage.SecretData{
		keyAccountName: []byte("myaccount"),
		keyAccountKey:  []byte(validKey),
	}, logr.Discard())
	if err == nil {
		t.Fatal("expected error for missing container")
	}
}

func TestNew_Success(t *testing.T) {
	s, err := New("az-backup", storage.SecretData{
		keyAccountName: []byte("myaccount"),
		keyAccountKey:  []byte(validKey),
		keyContainer:   []byte("backups"),
	}, logr.Discard())
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	if got := s.Name(); got != "az-backup" {
		t.Errorf("Name() = %q, want %q", got, "az-backup")
	}
}

func TestNew_InvalidKey(t *testing.T) {
	_, err := New("test", storage.SecretData{
		keyAccountName: []byte("myaccount"),
		keyAccountKey:  []byte("not-valid-base64!!!"),
		keyContainer:   []byte("backups"),
	}, logr.Discard())
	if err == nil {
		t.Fatal("expected error for invalid (non-base64) account key")
	}
}

func TestNew_TrimWhitespace(t *testing.T) {
	s, err := New("test", storage.SecretData{
		keyAccountName: []byte("  myaccount  \n"),
		keyAccountKey:  []byte("  " + validKey + "  "),
		keyContainer:   []byte("  backups  "),
	}, logr.Discard())
	if err != nil {
		t.Fatalf("New() should trim whitespace: %v", err)
	}
	st := s.(*azureStorage)
	if st.container != "backups" {
		t.Errorf("container = %q, want %q (trimmed)", st.container, "backups")
	}
}

func TestNew_WithPathPrefix(t *testing.T) {
	s, err := New("test", storage.SecretData{
		keyAccountName: []byte("myaccount"),
		keyAccountKey:  []byte(validKey),
		keyContainer:   []byte("backups"),
		keyPrefix:      []byte("/cluster-prod/"),
	}, logr.Discard())
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	st := s.(*azureStorage)
	if st.pathPrefix != "/cluster-prod" {
		t.Errorf("pathPrefix = %q, want %q (trailing slash stripped)", st.pathPrefix, "/cluster-prod")
	}
}

func TestFull_NoPrefix(t *testing.T) {
	a := &azureStorage{pathPrefix: ""}
	cases := []struct{ in, want string }{
		{"backups/dump.sql", "backups/dump.sql"},
		{"/backups/dump.sql", "backups/dump.sql"},
	}
	for _, c := range cases {
		if got := a.full(c.in); got != c.want {
			t.Errorf("full(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFull_WithPrefix(t *testing.T) {
	a := &azureStorage{pathPrefix: "cluster-prod"}
	cases := []struct{ in, want string }{
		{"target/dump.sql", "cluster-prod/target/dump.sql"},
		{"/target/dump.sql", "cluster-prod/target/dump.sql"},
	}
	for _, c := range cases {
		if got := a.full(c.in); got != c.want {
			t.Errorf("full(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStripPrefix_WithPrefix(t *testing.T) {
	a := &azureStorage{pathPrefix: "cluster-prod"}
	cases := []struct{ in, want string }{
		{"cluster-prod/target/dump.sql", "target/dump.sql"},
		{"cluster-prod/target/meta.json", "target/meta.json"},
	}
	for _, c := range cases {
		if got := a.stripPrefix(c.in); got != c.want {
			t.Errorf("stripPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
