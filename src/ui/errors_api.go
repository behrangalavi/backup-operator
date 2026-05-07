package ui

import "net/http"

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
