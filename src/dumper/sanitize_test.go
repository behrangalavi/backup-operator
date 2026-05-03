package dumper

import (
	"errors"
	"strings"
	"testing"
)

func TestSanitizeStderr_MasksLiteralSecret(t *testing.T) {
	in := "auth failed for user 'backup' with password 's3cret-PASSWORD!'"
	out := SanitizeStderr(in, "s3cret-PASSWORD!")
	if strings.Contains(out, "s3cret-PASSWORD!") {
		t.Errorf("password leaked: %q", out)
	}
	if !strings.Contains(out, "***") {
		t.Errorf("expected mask in %q", out)
	}
}

func TestSanitizeStderr_MasksMongoURI(t *testing.T) {
	in := `Failed to connect: mongodb://admin:hunter2@mongo.prod.svc:27017/?authSource=admin`
	out := SanitizeStderr(in)
	if strings.Contains(out, "hunter2") {
		t.Errorf("password leaked: %q", out)
	}
	if !strings.Contains(out, "mongodb://admin:***@") {
		t.Errorf("expected scheme://user:*** mask in %q", out)
	}
}

func TestSanitizeStderr_MasksPostgresURI(t *testing.T) {
	in := `error: postgres://app_user:p%40ss@db:5432/mydb sslmode=require failed`
	out := SanitizeStderr(in)
	if strings.Contains(out, "p%40ss") {
		t.Errorf("password leaked: %q", out)
	}
}

func TestSanitizeStderr_MasksPasswordKeyValue(t *testing.T) {
	in := `connect: dbname=app user=svc password=topsecret host=db port=5432`
	out := SanitizeStderr(in)
	if strings.Contains(out, "topsecret") {
		t.Errorf("password=… leaked: %q", out)
	}
	if !strings.Contains(out, "password=***") {
		t.Errorf("expected password=*** in %q", out)
	}
}

func TestSanitizeStderr_NoSecretsAndNoPattern(t *testing.T) {
	in := "Connection refused on host db.internal:5432"
	out := SanitizeStderr(in)
	if out != in {
		t.Errorf("benign stderr should pass through unchanged, got %q", out)
	}
}

func TestSanitizeStderr_EmptySecretsIgnored(t *testing.T) {
	out := SanitizeStderr("hello world", "", "")
	if out != "hello world" {
		t.Errorf("empty secrets should be ignored, got %q", out)
	}
}

func TestWrapExecError_IncludesSanitizedStderr(t *testing.T) {
	base := errors.New("exit status 1")
	err := WrapExecError("pg_dump", base, "FATAL: password authentication failed for user 'svc' (password=p123)", "p123")
	msg := err.Error()
	if strings.Contains(msg, "p123") {
		t.Errorf("literal secret leaked: %s", msg)
	}
	if !strings.Contains(msg, "pg_dump failed:") {
		t.Errorf("missing tool prefix: %s", msg)
	}
	if !errors.Is(err, base) {
		t.Errorf("wrapped error should chain via errors.Is")
	}
}

func TestWrapExecError_EmptyStderrOmitsColon(t *testing.T) {
	base := errors.New("exit status 2")
	err := WrapExecError("redis-cli", base, "")
	if want := "redis-cli failed: exit status 2"; err.Error() != want {
		t.Errorf("got %q, want %q", err.Error(), want)
	}
}
