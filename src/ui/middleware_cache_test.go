package ui

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fixedHandler writes a known body so tests can reason about ETag
// equality and gzip transparency. The body is large enough to clear
// the 1 KiB gzip threshold.
func fixedHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
}

func TestCachedJSON_EmitsETagAndCacheControl(t *testing.T) {
	body := strings.Repeat(`{"target":"x"},`, 100) // ~1.5 KB
	srv := httptest.NewServer(cachedJSON(fixedHandler(body)))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get("ETag") == "" {
		t.Error("ETag header must be set")
	}
	if !strings.Contains(resp.Header.Get("Cache-Control"), "max-age=5") {
		t.Errorf("Cache-Control should include max-age=5, got %q", resp.Header.Get("Cache-Control"))
	}
	if !strings.Contains(resp.Header.Get("Vary"), "Accept-Encoding") {
		t.Errorf("Vary should include Accept-Encoding, got %q", resp.Header.Get("Vary"))
	}
}

func TestCachedJSON_IfNoneMatchReturns304(t *testing.T) {
	body := strings.Repeat(`x`, 1500)
	srv := httptest.NewServer(cachedJSON(fixedHandler(body)))
	defer srv.Close()

	first, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Body.Close()
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("first request must set ETag")
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("If-None-Match", etag)
	second, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Body.Close() }()
	if second.StatusCode != http.StatusNotModified {
		t.Errorf("expected 304, got %d", second.StatusCode)
	}
	read, _ := io.ReadAll(second.Body)
	if len(read) != 0 {
		t.Errorf("304 body must be empty, got %d bytes", len(read))
	}
}

func TestCachedJSON_GzipWhenAccepted(t *testing.T) {
	body := strings.Repeat(`{"target":"prod-users"},`, 200) // ~5 KB
	srv := httptest.NewServer(cachedJSON(fixedHandler(body)))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected Content-Encoding=gzip, got %q", resp.Header.Get("Content-Encoding"))
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("response is not valid gzip: %v", err)
	}
	got, _ := io.ReadAll(gz)
	if string(got) != body {
		t.Errorf("decompressed body mismatch (got %d bytes vs %d)", len(got), len(body))
	}
}

func TestCachedJSON_SkipsGzipForSmallBody(t *testing.T) {
	body := `{"ok":true}` // <1 KB
	srv := httptest.NewServer(cachedJSON(fixedHandler(body)))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get("Content-Encoding") == "gzip" {
		t.Error("small bodies should not be gzipped — overhead exceeds savings")
	}
}

func TestCachedJSON_DoesNotCacheErrorResponses(t *testing.T) {
	srv := httptest.NewServer(cachedJSON(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
	})))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status passthrough: got %d", resp.StatusCode)
	}
	if resp.Header.Get("ETag") != "" {
		t.Error("error responses must not get an ETag — caching them masks recovery")
	}
	if resp.Header.Get("Cache-Control") != "" {
		t.Errorf("error responses must not set Cache-Control, got %q", resp.Header.Get("Cache-Control"))
	}
}

func TestCachedJSON_PassesThroughNonGET(t *testing.T) {
	called := false
	srv := httptest.NewServer(cachedJSON(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = w.Write([]byte("ok"))
	})))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(""))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !called {
		t.Fatal("handler must be invoked even for non-GET requests")
	}
	if resp.Header.Get("ETag") != "" {
		t.Error("non-GET responses must not be tagged — POST/PUT/DELETE results aren't conditionally cacheable here")
	}
}

func TestEtagMatches(t *testing.T) {
	cases := []struct {
		header, etag string
		want         bool
	}{
		{`"abc"`, `"abc"`, true},
		{`"abc", "def"`, `"def"`, true},
		{` "abc" , "def" `, `"abc"`, true},
		{`*`, `"abc"`, true},
		{`"abc"`, `"def"`, false},
		{``, `"abc"`, false},
	}
	for _, c := range cases {
		if got := etagMatches(c.header, c.etag); got != c.want {
			t.Errorf("etagMatches(%q, %q) = %v, want %v", c.header, c.etag, got, c.want)
		}
	}
}
