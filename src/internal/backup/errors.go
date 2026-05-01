package backup

import "fmt"

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
