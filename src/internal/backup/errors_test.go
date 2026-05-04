package backup

import (
	"errors"
	"fmt"
	"testing"
)

func TestRetryableError(t *testing.T) {
	inner := fmt.Errorf("connection reset")
	err := &RetryableError{Op: "upload", Err: inner}

	if !errors.Is(err, inner) {
		t.Error("Unwrap should expose the inner error")
	}
	want := "upload (retryable): connection reset"
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}

	var re *RetryableError
	if !errors.As(err, &re) {
		t.Error("errors.As should match *RetryableError")
	}
}

func TestPermanentError(t *testing.T) {
	inner := fmt.Errorf("invalid credentials")
	err := &PermanentError{Op: "init storage", Err: inner}

	if !errors.Is(err, inner) {
		t.Error("Unwrap should expose the inner error")
	}

	var pe *PermanentError
	if !errors.As(err, &pe) {
		t.Error("errors.As should match *PermanentError")
	}
}

func TestValidationError(t *testing.T) {
	err := &ValidationError{Field: "schedule", Message: "invalid cron"}
	want := "validation: schedule: invalid cron"
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

func TestClassifyUploadError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantPerm bool
	}{
		{"nil error", nil, false},
		{"no space left on device", fmt.Errorf("write /data/dump.gz: no space left on device"), true},
		{"disk quota exceeded", fmt.Errorf("sftp: Disk quota exceeded"), true},
		{"permission denied", fmt.Errorf("sftp: Permission denied"), true},
		{"access denied S3", fmt.Errorf("s3 upload: AccessDenied: Access Denied"), true},
		{"insufficient storage", fmt.Errorf("Insufficient storage space"), true},
		{"connection reset (retryable)", fmt.Errorf("connection reset by peer"), false},
		{"timeout (retryable)", fmt.Errorf("i/o timeout"), false},
		{"EOF (retryable)", fmt.Errorf("unexpected EOF"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := classifyUploadError("upload", tt.err)
			if tt.err == nil {
				if result != nil {
					t.Fatalf("expected nil, got %v", result)
				}
				return
			}
			var pe *PermanentError
			var re *RetryableError
			if tt.wantPerm {
				if !errors.As(result, &pe) {
					t.Errorf("expected PermanentError for %q, got %T", tt.err, result)
				}
			} else {
				if !errors.As(result, &re) {
					t.Errorf("expected RetryableError for %q, got %T", tt.err, result)
				}
			}
		})
	}
}
