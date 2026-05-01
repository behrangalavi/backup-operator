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
