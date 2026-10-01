package storage

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestContextReader_StopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := ContextReader(ctx, strings.NewReader("hello world"))
	buf := make([]byte, 5)
	if n, err := r.Read(buf); err != nil || n != 5 {
		t.Fatalf("first read = %d, %v", n, err)
	}
	cancel()
	if _, err := r.Read(buf); !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancel must return ctx.Err(), got %v", err)
	}
}

func TestContextReader_PassThrough(t *testing.T) {
	got, err := io.ReadAll(ContextReader(context.Background(), strings.NewReader("abc")))
	if err != nil || string(got) != "abc" {
		t.Fatalf("ReadAll = %q, %v", got, err)
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(errors.Join(errors.New("ctx"), ErrNotFound)) {
		t.Fatal("wrapped ErrNotFound must be detected")
	}
	if IsNotFound(errors.New("other")) {
		t.Fatal("unrelated error must not be not-found")
	}
}
