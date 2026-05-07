package safe

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"
)

func TestGoroutine_RecoversAndLogs(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	log := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		buf.WriteString(args)
		buf.WriteByte('\n')
	}, funcr.Options{Verbosity: 0})

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer Goroutine(log, "test-phase", "test-key")
		panic("boom")
	}()
	<-done

	out := buf.String()
	if !strings.Contains(out, "goroutine recovered") {
		t.Errorf("expected log message %q in output, got %q", "goroutine recovered", out)
	}
	if !strings.Contains(out, "test-phase") {
		t.Errorf("expected phase in log output, got %q", out)
	}
	if !strings.Contains(out, "test-key") {
		t.Errorf("expected key in log output, got %q", out)
	}
	if !strings.Contains(out, "stack") {
		t.Errorf("expected stack trace key in log output, got %q", out)
	}
	// The stack must include this test function — proves the trace is
	// real and not just the literal word "stack".
	if !strings.Contains(out, "TestGoroutine_RecoversAndLogs") {
		t.Errorf("stack trace should reference the test function, got %q", out)
	}
}

func TestGoroutine_NoPanicNoLog(t *testing.T) {
	var buf bytes.Buffer
	log := funcr.New(func(prefix, args string) {
		buf.WriteString(args)
	}, funcr.Options{Verbosity: 0})

	func() {
		defer Goroutine(log, "test", "key")
	}()

	if buf.Len() != 0 {
		t.Errorf("expected no log output when no panic, got %q", buf.String())
	}
}
