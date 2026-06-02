package ui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestSanitizeStorageError(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "keeps protocol diagnostic",
			in:          "upload: 550 Permission denied",
			wantContain: []string{"550 Permission denied"},
		},
		{
			name:        "scrubs URI userinfo",
			in:          "dial sftp://backupuser:s3cr3t@nas.internal:23: connection refused",
			wantContain: []string{"sftp://***@nas.internal:23", "connection refused"},
			wantAbsent:  []string{"s3cr3t", "backupuser"},
		},
		{
			name:        "scrubs key=value password",
			in:          "auth failed: password=hunter2 rejected",
			wantContain: []string{"password=***", "rejected"},
			wantAbsent:  []string{"hunter2"},
		},
		{
			name:        "scrubs access-key-id",
			in:          "config error access-key-id=AKIAIOSFODNN7EXAMPLE invalid",
			wantContain: []string{"access-key-id=***"},
			wantAbsent:  []string{"AKIAIOSFODNN7EXAMPLE"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeStorageError(errors.New(c.in))
			for _, want := range c.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("sanitized %q missing %q", got, want)
				}
			}
			for _, absent := range c.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("sanitized %q still leaks %q", got, absent)
				}
			}
		})
	}
	if sanitizeStorageError(nil) != "" {
		t.Error("nil error should sanitize to empty string")
	}
}

func TestCSRFMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := csrfMiddleware(next)

	cases := []struct {
		name    string
		method  string
		headers map[string]string
		want    int
	}{
		{"GET always allowed", http.MethodGet, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusOK},
		{"POST same-origin allowed", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"POST none allowed", http.MethodPost, map[string]string{"Sec-Fetch-Site": "none"}, http.StatusOK},
		{"POST cross-site rejected", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"DELETE same-site rejected", http.MethodDelete, map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"non-browser client (no headers) allowed", http.MethodPost, nil, http.StatusOK},
		{"matching Origin allowed", http.MethodPost, map[string]string{"Origin": "http://example.test"}, http.StatusOK},
		{"mismatched Origin rejected", http.MethodPost, map[string]string{"Origin": "http://evil.test"}, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, "http://example.test/api/sources", nil)
			req.Host = "example.test"
			for k, v := range c.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d", rec.Code, c.want)
			}
		})
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub ||
		(len(s) > 0 && (s[:len(sub)] == sub || contains(s[1:], sub))))
}
