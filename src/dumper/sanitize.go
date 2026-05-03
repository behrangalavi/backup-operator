package dumper

import (
	"fmt"
	"regexp"
	"strings"
)

// uriCredentialPattern matches "scheme://user:pass@host" so that any URI which
// happens to surface in stderr (mongodump, redis-cli, etc.) gets its
// credential pair masked in error output.
var uriCredentialPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+\-.]*://)([^:/@\s]+):([^@\s]+)@`)

// keyValuePassword matches `password=...`, `passwd=...`, `pwd=...`, `auth=...`
// up to the next whitespace, ampersand, or quote. Case-insensitive.
var keyValuePassword = regexp.MustCompile(`(?i)\b(password|passwd|pwd|auth)=([^\s&'"]+)`)

// SanitizeStderr scrubs known-sensitive substrings from a dump tool's stderr
// before that stderr ends up in error chains, logs, Kubernetes Events, or UI
// responses. It masks: any literal value passed in `secrets` (typically the
// raw password), embedded URI credentials, and `password=…` style
// key-value pairs. Use it everywhere we wrap an exec error with stderr text.
//
// The contract is fail-safe: an empty stderr returns "", we never panic, and
// missing patterns are simply left untouched.
func SanitizeStderr(stderr string, secrets ...string) string {
	if stderr == "" {
		return ""
	}
	out := stderr
	for _, s := range secrets {
		if s == "" {
			continue
		}
		out = strings.ReplaceAll(out, s, "***")
	}
	out = uriCredentialPattern.ReplaceAllString(out, "$1$2:***@")
	out = keyValuePassword.ReplaceAllString(out, "$1=***")
	return out
}

// WrapExecError builds a uniform "<tool> failed: <err>: <sanitized stderr>"
// error. Centralising this keeps every dumper consistent and ensures we never
// forget to sanitise when adding a new backend.
func WrapExecError(tool string, err error, stderr string, secrets ...string) error {
	clean := SanitizeStderr(stderr, secrets...)
	if clean == "" {
		return fmt.Errorf("%s failed: %w", tool, err)
	}
	return fmt.Errorf("%s failed: %w: %s", tool, err, clean)
}

// SanitizeError returns a new error whose message has known-sensitive content
// scrubbed. Use this when wrapping driver errors (pgx, mongo) where the
// original error message may itself echo the connection URI back. Trade-off:
// the returned error breaks `errors.Is` against the underlying driver error,
// because preserving %w would also preserve the leaky message verbatim. We
// pick "no leak" over "preserved chain" — at this layer the message is for
// humans, not programmatic matching.
func SanitizeError(prefix string, err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	clean := SanitizeStderr(err.Error(), secrets...)
	if prefix == "" {
		return fmt.Errorf("%s", clean)
	}
	return fmt.Errorf("%s: %s", prefix, clean)
}
