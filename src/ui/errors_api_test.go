package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteError_ShapeAndStatus(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		code     string
		message  string
	}{
		{"validation", http.StatusBadRequest, codeValidation, "name is required"},
		{"not_found", http.StatusNotFound, codeNotFound, "secret missing"},
		{"forbidden", http.StatusForbidden, codeForbidden, "read-only mode"},
		{"conflict", http.StatusConflict, codeConflict, "already exists"},
		{"server_error", http.StatusInternalServerError, codeInternal, "k8s api unreachable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeError(rec, c.status, c.code, c.message)
			if rec.Code != c.status {
				t.Errorf("status = %d, want %d", rec.Code, c.status)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			var out apiResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("invalid JSON body: %v", err)
			}
			if out.Code != c.code {
				t.Errorf("body.code = %q, want %q", out.Code, c.code)
			}
			if out.Message != c.message {
				t.Errorf("body.message = %q, want %q", out.Message, c.message)
			}
			if out.OK {
				t.Error("body.ok should be false on errors")
			}
		})
	}
}

func TestApiResponse_CodeOmittedWhenEmpty(t *testing.T) {
	// Success responses don't carry a code — verify it's omitted from JSON
	// rather than serialised as `"code": ""`. The frontend distinguishes
	// "had an error code" from "field absent".
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, apiResponse{OK: true, Name: "foo"})
	body := rec.Body.String()
	if contains(body, `"code"`) {
		t.Errorf("empty Code should be omitted from JSON, got: %s", body)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub ||
		(len(s) > 0 && (s[:len(sub)] == sub || contains(s[1:], sub))))
}
