package sftp

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"strings"
	"testing"

	"backup-operator/storage"

	"github.com/go-logr/logr"
	"golang.org/x/crypto/ssh"
)

// generateTestPrivateKey produces a fresh OpenSSH-formatted ed25519 key so
// the auth tests don't ship a static private key. Failures here are setup
// errors, not assertions.
func generateTestPrivateKey(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	return pem.EncodeToMemory(block)
}

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

func TestBuildAuthMethods_KeyOnly(t *testing.T) {
	pk := generateTestPrivateKey(t)
	auths, err := buildAuthMethods("test", storage.SecretData{
		keyPrivateKey: pk,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected exactly 1 auth method (public key), got %d", len(auths))
	}
}

func TestBuildAuthMethods_PasswordOnly(t *testing.T) {
	auths, err := buildAuthMethods("test", storage.SecretData{
		keyPassword: []byte("hunter2"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected exactly 1 auth method (password), got %d", len(auths))
	}
}

// When both are supplied the public-key method must come first so the SSH
// client tries it before falling back to password — that's the standard
// openssh ordering and what users intuitively expect.
func TestBuildAuthMethods_BothPrefersKeyFirst(t *testing.T) {
	pk := generateTestPrivateKey(t)
	auths, err := buildAuthMethods("test", storage.SecretData{
		keyPrivateKey: pk,
		keyPassword:   []byte("hunter2"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 2 {
		t.Fatalf("expected 2 auth methods, got %d", len(auths))
	}
}

func TestBuildAuthMethods_NeitherIsError(t *testing.T) {
	_, err := buildAuthMethods("test", storage.SecretData{})
	if err == nil {
		t.Fatal("expected error when neither password nor private key is supplied")
	}
	if !strings.Contains(err.Error(), "missing auth") {
		t.Errorf("error should mention missing auth, got: %v", err)
	}
}

func TestBuildAuthMethods_BadKeyIsError(t *testing.T) {
	_, err := buildAuthMethods("test", storage.SecretData{
		keyPrivateKey: []byte("not a real private key"),
	})
	if err == nil {
		t.Fatal("expected error for malformed private key")
	}
	if !strings.Contains(err.Error(), "parse private key") {
		t.Errorf("error should mention parse private key, got: %v", err)
	}
}
