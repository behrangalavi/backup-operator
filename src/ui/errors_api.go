package ui

import (
	"net/http"
	"regexp"
)

// Error codes returned in the apiResponse.Code field. Stable strings —
// the frontend matches on them to choose toast severity, error icons, and
// (eventually) translation lookups. Changing a constant value is a
// breaking change for the SPA.
const (
	codeBadRequest       = "bad_request"
	codeValidation       = "validation"
	codeMethodNotAllowed = "method_not_allowed"
	codeNotFound         = "not_found"
	codeConflict         = "conflict"
	codeForbidden        = "forbidden"
	codeInternal         = "server_error"
)

// writeError is the canonical way to send a 4xx/5xx JSON response. It tags
// the response with a stable error code so the frontend can route presentation
// (toast severity, retry hints, future i18n) without parsing message strings.
//
// Use writeError for any non-2xx response. Plain writeJSON should be reserved
// for success bodies — keep the two helpers cleanly separated so a future
// audit can grep for unsanitized error paths.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiResponse{Code: code, Message: message})
}

// Credential-bearing patterns that must never reach an (unauthenticated) UI
// client. We scrub these from storage-diagnostic errors but keep the rest of
// the message intact — the test-connection / destination-stats endpoints are
// explicitly diagnostic (§18), so collapsing every error to "operation failed"
// would destroy their reason for existing.
var (
	// scheme://user:pass@host  → keep scheme + host, drop userinfo
	reURICredentials = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@\s]*@`)
	// password=secret / pwd=… / auth=… / token=… / access-key=… (any case)
	reKVCredentials = regexp.MustCompile(`(?i)\b(pass(word|wd)?|pwd|auth|token|secret[-_]?(access[-_]?)?key|access[-_]?key([-_]?id)?)\s*[=:]\s*\S+`)
)

// sanitizeStorageError scrubs credential material from a storage backend error
// while preserving the protocol-level diagnostic that makes these endpoints
// useful (e.g. "550 Permission denied", "no space left on device",
// "connection refused"). Use it on the diagnostic endpoints; use sanitizeError
// (fully generic) on data-fetch paths where no diagnostic value is intended.
func sanitizeStorageError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	msg = reURICredentials.ReplaceAllString(msg, "${1}***@")
	msg = reKVCredentials.ReplaceAllString(msg, "${1}=***")
	return msg
}
