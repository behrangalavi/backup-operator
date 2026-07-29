package postgres

import (
	"testing"

	"backup-operator/dumper"

	"github.com/go-logr/logr"
)

func TestType(t *testing.T) {
	d := New(dumper.Config{}, logr.Discard())
	if got := d.Type(); got != "postgres" {
		t.Errorf("Type() = %q, want %q", got, "postgres")
	}
}

func TestConnString_Defaults(t *testing.T) {
	d := &postgresDumper{cfg: dumper.Config{
		Host:     "db.prod.svc",
		Port:     5432,
		Username: "appuser",
		Password: "s3cret",
		Database: "mydb",
	}}
	got := d.connString()
	// Must include all components.
	for _, want := range []string{
		"postgres://",
		"appuser:",
		"@db.prod.svc:5432",
		"/mydb",
		"sslmode=prefer",
	} {
		if !contains(got, want) {
			t.Errorf("connString() = %q, missing %q", got, want)
		}
	}
}

func TestSSLMode_SharedByStatsAndDump(t *testing.T) {
	// sslMode is the single source of truth the pg_dump env (PGSSLMODE) and
	// connString both read, so the dump path can't silently downgrade while
	// stats stays verified.
	def := New(dumper.Config{}, logr.Discard()).(*postgresDumper)
	if got := def.sslMode(); got != "prefer" {
		t.Errorf("default sslMode() = %q, want prefer", got)
	}
	strict := New(dumper.Config{Extra: map[string]string{"sslmode": "verify-full"}}, logr.Discard()).(*postgresDumper)
	if got := strict.sslMode(); got != "verify-full" {
		t.Errorf("sslMode() = %q, want verify-full", got)
	}
}

func TestConnString_SSLModeOverride(t *testing.T) {
	d := &postgresDumper{cfg: dumper.Config{
		Host:     "localhost",
		Port:     5432,
		Username: "u",
		Password: "p",
		Database: "d",
		Extra:    map[string]string{"sslmode": "require"},
	}}
	got := d.connString()
	if !contains(got, "sslmode=require") {
		t.Errorf("connString() = %q, expected sslmode=require", got)
	}
	if contains(got, "sslmode=prefer") {
		t.Errorf("connString() = %q, should not contain default sslmode=prefer when overridden", got)
	}
}

func TestConnString_PasswordEscaping(t *testing.T) {
	d := &postgresDumper{cfg: dumper.Config{
		Host:     "localhost",
		Port:     5432,
		Username: "u",
		Password: "p@ss:word/special",
		Database: "d",
	}}
	got := d.connString()
	// Password with special characters must be URL-encoded.
	if contains(got, "p@ss:word/special") {
		t.Errorf("connString() = %q, password should be URL-encoded", got)
	}
}

func TestConnString_EmptySSLMode(t *testing.T) {
	d := &postgresDumper{cfg: dumper.Config{
		Host:     "localhost",
		Port:     5432,
		Username: "u",
		Password: "p",
		Database: "d",
		Extra:    map[string]string{"sslmode": ""},
	}}
	got := d.connString()
	// Empty sslmode should fall back to "prefer".
	if !contains(got, "sslmode=prefer") {
		t.Errorf("connString() = %q, expected sslmode=prefer for empty override", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		containsHelper(s, sub))
}

func containsHelper(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
