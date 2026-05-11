package sftp

import (
	"strings"
	"testing"

	"github.com/go-logr/logr"
)

func TestBuildHostKeyCallback_EmptyData_RejectsWithoutOptIn(t *testing.T) {
	_, err := buildHostKeyCallback("test", nil, false, logr.Discard())
	if err == nil {
		t.Fatal("expected error when known-hosts is empty and insecure is not enabled")
	}
	if !strings.Contains(err.Error(), "insecure-skip-host-verify") {
		t.Errorf("error should mention insecure-skip-host-verify, got: %v", err)
	}
}

func TestBuildHostKeyCallback_EmptyData_FallsBackToInsecureWhenOptedIn(t *testing.T) {
	cb, err := buildHostKeyCallback("test", nil, true, logr.Discard())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cb == nil {
		t.Fatal("expected a non-nil callback when insecure opt-in is set")
	}
}

func TestBuildHostKeyCallback_ValidKnownHosts(t *testing.T) {
	// One arbitrary but well-formed known_hosts line. The knownhosts parser
	// validates structure; the actual key bytes are opaque to it.
	const sample = "[example.com]:22 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDXfXTRY9k5y3w8ZqbmFTtXfTfnj1l1QPFJpVwa2PiTI\n"
	cb, err := buildHostKeyCallback("test", []byte(sample), false, logr.Discard())
	if err != nil {
		t.Fatalf("expected valid known-hosts to parse, got: %v", err)
	}
	if cb == nil {
		t.Fatal("expected a non-nil callback for valid known-hosts")
	}
}

func TestBuildHostKeyCallback_MalformedKnownHosts_ReturnsError(t *testing.T) {
	// Total garbage — not even close to known_hosts shape.
	junk := []byte("this is not a known_hosts file at all\n@@@\n")
	_, err := buildHostKeyCallback("test", junk, false, logr.Discard())
	if err == nil {
		t.Fatal("expected malformed known-hosts to return an error")
	}
	if !strings.Contains(err.Error(), "known-hosts") {
		t.Errorf("error should mention known-hosts, got: %v", err)
	}
}
