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

// TestNew_RejectsSSRFAccountName regresses the SSRF bypass: the account name
// is interpolated into the service-URL host, so a value that smuggles a host
// (metadata IP, path segment) must be rejected before a client is built.
func TestNew_RejectsSSRFAccountName(t *testing.T) {
	bad := []string{
		"169.254.169.254/",                    // metadata host injection
		"evil.example.com",                    // dots not allowed
		"acct/../x",                           // path traversal
		"ACCOUNT",                             // uppercase (Azure is lowercase-only)
		"ab",                                  // too short
		"this-name-is-way-too-long-for-azure", // >24 + hyphen
		"my_account",                          // underscore
	}
	for _, a := range bad {
		_, err := New("test", storage.SecretData{
			keyAccountName: []byte(a),
			keyAccountKey:  []byte(validKey),
			keyContainer:   []byte("backups"),
		}, logr.Discard())
		if err == nil {
			t.Errorf("account-name %q must be rejected", a)
		}
	}
	// A valid account name still succeeds.
	if _, err := New("test", storage.SecretData{
		keyAccountName: []byte("myaccount123"),
		keyAccountKey:  []byte(validKey),
		keyContainer:   []byte("backups"),
	}, logr.Discard()); err != nil {
		t.Errorf("valid account name rejected: %v", err)
	}
}

// TestFull_ListPrefixKeepsTrailingSlash is the regression guard for the
// prefix-bleed bug: a List prefix of "db/" must stay "prefix/db/" and not
// collapse to "prefix/db", which would also match the sibling target
// "prefix/db-archive/..." and let one target's retention delete another's.
func TestFull_ListPrefixKeepsTrailingSlash(t *testing.T) {
	s := &azureStorage{pathPrefix: "cluster-prod"}
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
	s := &azureStorage{pathPrefix: "prod"}
	if got := s.full(""); got != "prod/" {
		t.Fatalf("full(%q) = %q, want %q", "", got, "prod/")
	}
	if got := s.stripPrefix("prod-eu/db/x"); got != "prod-eu/db/x" {
		t.Fatalf("stripPrefix must only strip the exact %q boundary, got %q", "prod/", got)
	}
}
