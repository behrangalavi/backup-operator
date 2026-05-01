package crypto

import (
	"bytes"
	"io"
	"testing"
)

func TestRoundtrip(t *testing.T) {
	// Generate a fresh key pair via the age library.
	const pub = "age1g5hdv6wq0fgph462wpwtgm44vhjjex9xam27s0qsrhwzrfmyxcrs59qd48"
	const priv = "AGE-SECRET-KEY-12UEPM4Z84JZJ3ZRNJE8GY8LR8R00MLADG4F4VHKHYVGGPYTURTZS7LGSUJ"

	enc, err := NewFromPublicKeys(pub)
	if err != nil {
		t.Fatalf("NewFromPublicKeys: %v", err)
	}

	plain := []byte("hello backup-operator")
	var cipherBuf bytes.Buffer
	wc, err := enc.Wrap(&cipherBuf)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if _, err := wc.Write(plain); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := wc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	dec, err := NewDecryptorFromKeys(priv)
	if err != nil {
		t.Fatalf("NewDecryptorFromKeys: %v", err)
	}
	reader, err := dec.Wrap(&cipherBuf)
	if err != nil {
		t.Fatalf("Decrypt Wrap: %v", err)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("roundtrip mismatch: got %q, want %q", got, plain)
	}
}

func TestNewFromPublicKeys_Empty(t *testing.T) {
	_, err := NewFromPublicKeys("")
	if err == nil {
		t.Error("expected error for empty keys")
	}
}

func TestNewFromPublicKeys_Invalid(t *testing.T) {
	_, err := NewFromPublicKeys("not-a-real-key")
	if err == nil {
		t.Error("expected error for invalid key")
	}
}

func TestNewFromPublicKeys_CommentsIgnored(t *testing.T) {
	const pub = "age1g5hdv6wq0fgph462wpwtgm44vhjjex9xam27s0qsrhwzrfmyxcrs59qd48"
	input := "# comment line\n" + pub + "\n\n# another comment\n"
	enc, err := NewFromPublicKeys(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if enc == nil {
		t.Error("expected non-nil encryptor")
	}
}

func TestNewDecryptorFromKeys_Empty(t *testing.T) {
	_, err := NewDecryptorFromKeys("")
	if err == nil {
		t.Error("expected error for empty keys")
	}
}

func TestNewDecryptorFromKeys_Invalid(t *testing.T) {
	_, err := NewDecryptorFromKeys("not-a-private-key")
	if err == nil {
		t.Error("expected error for invalid key")
	}
}
