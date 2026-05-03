package ui

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLimitBodyMiddleware_RejectsOversize(t *testing.T) {
	const max = 32
	got := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err == nil {
			t.Error("expected MaxBytesReader to surface an error on oversize body")
		}
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	})
	srv := httptest.NewServer(limitBodyMiddleware(max, got))
	defer srv.Close()

	body := bytes.Repeat([]byte("A"), max*4)
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

func TestLimitBodyMiddleware_AllowsSmallBody(t *testing.T) {
	const max = 1024
	called := false
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if len(b) != 5 {
			t.Errorf("read %d bytes, want 5", len(b))
		}
		called = true
	})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte("hello")))
	rec := httptest.NewRecorder()
	limitBodyMiddleware(max, h).ServeHTTP(rec, req)
	if !called {
		t.Error("handler was not called")
	}
}

func TestSSEBroker_RejectsOverCap(t *testing.T) {
	b := newSSEBroker()
	b.maxClients = 2
	a := b.subscribe()
	if a == nil {
		t.Fatal("first subscribe should succeed")
	}
	c := b.subscribe()
	if c == nil {
		t.Fatal("second subscribe should succeed")
	}
	if extra := b.subscribe(); extra != nil {
		t.Error("third subscribe should be refused at cap")
	}
	// After unsubscribing one, a new subscriber should fit again.
	b.unsubscribe(a)
	if again := b.subscribe(); again == nil {
		t.Error("subscribe after unsubscribe should succeed")
	}
}

func TestSSEBroker_NoCapMeansUnlimited(t *testing.T) {
	b := newSSEBroker() // maxClients = 0
	for i := 0; i < 100; i++ {
		if b.subscribe() == nil {
			t.Fatalf("subscriber %d refused with no cap set", i)
		}
	}
}
