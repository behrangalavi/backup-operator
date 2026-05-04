package backup

import (
	"fmt"
	"strings"
)

// RetryableError signals a transient failure (e.g. network timeout) where
// retrying the same operation later may succeed.
type RetryableError struct {
	Op  string // "upload", "list", "delete", ...
	Err error
}

func (e *RetryableError) Error() string { return fmt.Sprintf("%s (retryable): %s", e.Op, e.Err) }
func (e *RetryableError) Unwrap() error { return e.Err }

// PermanentError signals a failure that will not resolve by retrying, such as
// a missing credential or an invalid configuration.
type PermanentError struct {
	Op  string
	Err error
}

func (e *PermanentError) Error() string { return fmt.Sprintf("%s (permanent): %s", e.Op, e.Err) }
func (e *PermanentError) Unwrap() error { return e.Err }

// ValidationError signals invalid user-supplied configuration — distinct from
// runtime failures so callers can surface it differently (e.g. no retry, alert
// on config).
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation: %s: %s", e.Field, e.Message)
}

// permanentErrorSubstrings contains error message fragments that indicate a
// failure will not resolve by retrying — typically resource exhaustion or
// permission issues. Matched case-insensitively.
var permanentErrorSubstrings = []string{
	"no space left on device",
	"disk quota exceeded",
	"quota exceeded",
	"permission denied",
	"access denied",
	"accessdenied",
	"insufficient storage",
	"not enough space",
	"storagefull",
}

// classifyUploadError inspects an upload error and returns either a
// PermanentError (for disk-full, quota, permission issues) or a
// RetryableError (for everything else). This prevents wasting retry
// attempts against a full or inaccessible storage backend.
func classifyUploadError(op string, err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	for _, sub := range permanentErrorSubstrings {
		if strings.Contains(msg, sub) {
			return &PermanentError{Op: op, Err: err}
		}
	}
	return &RetryableError{Op: op, Err: err}
}
