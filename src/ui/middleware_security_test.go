package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"
)

func TestSecurityHeaders_PresentOnEveryResponse(t *testing.T) {
	h := securityHeadersMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/targets", nil))
	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"script-src 'self'", "frame-ancestors 'none'", "object-src 'none'", "connect-src 'self'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP missing %q: %s", directive, csp)
		}
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") || strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Errorf("CSP must not allow inline scripts: %s", csp)
	}
	if rec.Header().Get("Permissions-Policy") == "" {
		t.Error("Permissions-Policy missing")
	}
}

func TestRequestContext_IDPassthroughAndGeneration(t *testing.T) {
	var seenID, seenCaller string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenID = requestIDFromContext(r.Context())
		seenCaller = callerFromContext(r.Context())
		w.WriteHeader(204)
	})
	h := requestContextMiddleware(logr.Discard(), inner)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	req.Header.Set("X-Request-Id", "abc-123.x")
	req.Header.Set("X-Forwarded-Email", " alice@example.com\x00\n")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seenID != "abc-123.x" || rec.Header().Get("X-Request-Id") != "abc-123.x" {
		t.Errorf("valid client request id must pass through, got ctx=%q header=%q", seenID, rec.Header().Get("X-Request-Id"))
	}
	if seenCaller != "alice@example.com" {
		t.Errorf("caller identity must be trimmed and stripped of control chars, got %q", seenCaller)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	req.Header.Set("X-Request-Id", "evil\nvalue; drop")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seenID == "" || strings.ContainsAny(seenID, "\n; ") || seenID == "evil\nvalue; drop" {
		t.Errorf("hostile request id must be replaced by a generated one, got %q", seenID)
	}
	if rec.Header().Get("X-Request-Id") != seenID {
		t.Error("generated id must be echoed in the response header")
	}
}

func TestCallerIdentity_PrecedenceAndCap(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Auth-Request-Email", "second@example.com")
	req.Header.Set("X-Forwarded-User", "first")
	if got := callerIdentity(req); got != "first" {
		t.Errorf("X-Forwarded-User must win, got %q", got)
	}
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-User", strings.Repeat("a", 500))
	if got := callerIdentity(req); len(got) != maxIdentityLen {
		t.Errorf("identity must be capped at %d, got %d", maxIdentityLen, len(got))
	}
	if got := callerIdentity(httptest.NewRequest(http.MethodGet, "/", nil)); got != "" {
		t.Errorf("no headers → empty identity, got %q", got)
	}
}

func TestAuditSuffix(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxKeyRequestID, "rid")
	ctx = context.WithValue(ctx, ctxKeyCaller, "bob")
	if got := auditSuffix(ctx); got != " (by bob, request rid)" {
		t.Errorf("auditSuffix = %q", got)
	}
	if got := auditSuffix(context.Background()); got != "" {
		t.Errorf("empty context must add nothing, got %q", got)
	}
}

func TestNoDirListing(t *testing.T) {
	served := false
	h := noDirListing(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { served = true; w.WriteHeader(200) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/js/", nil))
	if rec.Code != http.StatusNotFound || served {
		t.Errorf("directory URL must 404 without reaching the file server, got %d served=%v", rec.Code, served)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/js/core.js", nil))
	if rec.Code != 200 || !served {
		t.Errorf("file URL must be served, got %d", rec.Code)
	}
}
