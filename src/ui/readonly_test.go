package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadOnlyMiddleware_Disabled_PassesThrough(t *testing.T) {
	called := false
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	req := httptest.NewRequest(http.MethodPost, "/api/trigger/x", nil)
	rec := httptest.NewRecorder()
	readOnlyMiddleware(false, h).ServeHTTP(rec, req)
	if !called {
		t.Error("handler must be called when read-only is disabled")
	}
}

func TestReadOnlyMiddleware_BlocksMutations(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("handler must not run for %s in read-only mode", r.Method)
	})
	mw := readOnlyMiddleware(true, h)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(m, "/api/destinations/x", nil)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", m, rec.Code)
		}
	}
}

func TestReadOnlyMiddleware_AllowsSafeMethods(t *testing.T) {
	mw := readOnlyMiddleware(true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		req := httptest.NewRequest(m, "/api/targets", nil)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", m, rec.Code)
		}
	}
}
