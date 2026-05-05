package crypto

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// drPubKey / drPrivKey is a static keypair used as the long-lived "DR"
// recipient in tests. Same constants as age_test.go, kept distinct for
// clarity at call sites.
const (
	drPubKey  = "age1g5hdv6wq0fgph462wpwtgm44vhjjex9xam27s0qsrhwzrfmyxcrs59qd48"
	drPrivKey = "AGE-SECRET-KEY-12UEPM4Z84JZJ3ZRNJE8GY8LR8R00MLADG4F4VHKHYVGGPYTURTZS7LGSUJ"
)

func TestGenerateEphemeralIdentity_PublicLineNonEmpty(t *testing.T) {
	eph, err := GenerateEphemeralIdentity()
	if err != nil {
		t.Fatalf("GenerateEphemeralIdentity: %v", err)
	}
	pub := eph.PublicLine()
	if pub == "" {
		t.Fatal("PublicLine returned empty")
	}
	if !strings.HasPrefix(pub, "age1") {
		t.Errorf("PublicLine = %q, want age1... prefix", pub)
	}
}

func TestEphemeralIdentity_FingerprintStable(t *testing.T) {
	eph, err := GenerateEphemeralIdentity()
	if err != nil {
		t.Fatal(err)
	}
	a := eph.RecipientFingerprint()
	b := eph.RecipientFingerprint()
	if a != b {
		t.Errorf("fingerprint not stable: %q vs %q", a, b)
	}
	if len(a) != 16 {
		t.Errorf("fingerprint len = %d, want 16 hex chars", len(a))
	}
}

func TestEphemeralIdentity_Wipe(t *testing.T) {
	eph, err := GenerateEphemeralIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if eph.PublicLine() == "" {
		t.Fatal("public line empty before wipe")
	}
	eph.Wipe()
	if eph.PublicLine() != "" {
		t.Error("PublicLine should return empty after Wipe")
	}
	if _, err := eph.Decryptor(); err == nil {
		t.Error("Decryptor should fail after Wipe")
	}
}

// Roundtrip with both DR + ephemeral recipients: the resulting ciphertext
// must decrypt with EITHER identity, independently. This is the core
// security property the verifier relies on — the DR key keeps working
// after the ephemeral half is wiped.
func TestEncryptorWithExtraRecipient_BothCanDecrypt(t *testing.T) {
	base, err := NewFromPublicKeys(drPubKey)
	if err != nil {
		t.Fatal(err)
	}
	eph, err := GenerateEphemeralIdentity()
	if err != nil {
		t.Fatal(err)
	}

	combined, err := NewEncryptorWithExtraRecipient(base, eph.PublicLine())
	if err != nil {
		t.Fatalf("NewEncryptorWithExtraRecipient: %v", err)
	}

	plain := []byte("encrypted with two recipients")
	var cipherBuf bytes.Buffer
	wc, err := combined.Wrap(&cipherBuf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wc.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := wc.Close(); err != nil {
		t.Fatal(err)
	}
	cipher := cipherBuf.Bytes()

	// Path 1: DR private decrypts.
	drDec, err := NewDecryptorFromKeys(drPrivKey)
	if err != nil {
		t.Fatal(err)
	}
	r, err := drDec.Wrap(bytes.NewReader(cipher))
	if err != nil {
		t.Fatalf("DR decrypt wrap: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("DR roundtrip mismatch: %q vs %q", got, plain)
	}

	// Path 2: ephemeral private decrypts the same ciphertext.
	ephDec, err := eph.Decryptor()
	if err != nil {
		t.Fatal(err)
	}
	r2, err := ephDec.Wrap(bytes.NewReader(cipher))
	if err != nil {
		t.Fatalf("ephemeral decrypt wrap: %v", err)
	}
	got2, err := io.ReadAll(r2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got2, plain) {
		t.Errorf("ephemeral roundtrip mismatch: %q vs %q", got2, plain)
	}
}

// Empty extraPublic → returned encryptor is the base itself, so the
// caller can use the function unconditionally.
func TestEncryptorWithExtraRecipient_EmptyReturnsBase(t *testing.T) {
	base, err := NewFromPublicKeys(drPubKey)
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewEncryptorWithExtraRecipient(base, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != base {
		t.Error("empty extra recipient should return base encryptor unchanged")
	}
	got2, err := NewEncryptorWithExtraRecipient(base, "   \n   ")
	if err != nil {
		t.Fatal(err)
	}
	if got2 != base {
		t.Error("whitespace-only extra recipient should also return base unchanged")
	}
}

func TestEncryptorWithExtraRecipient_InvalidRejected(t *testing.T) {
	base, err := NewFromPublicKeys(drPubKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewEncryptorWithExtraRecipient(base, "not-a-real-key"); err == nil {
		t.Error("expected error for invalid extra recipient")
	}
}

// After wiping the ephemeral identity, the DR identity must still decrypt.
// This proves the security model: cluster-compromise that grabs the
// ephemeral private affects only one run; the DR key (kept offline)
// retains universal access.
func TestEphemeralWipe_DRKeyStillDecrypts(t *testing.T) {
	base, err := NewFromPublicKeys(drPubKey)
	if err != nil {
		t.Fatal(err)
	}
	eph, err := GenerateEphemeralIdentity()
	if err != nil {
		t.Fatal(err)
	}
	combined, err := NewEncryptorWithExtraRecipient(base, eph.PublicLine())
	if err != nil {
		t.Fatal(err)
	}

	var cipherBuf bytes.Buffer
	wc, _ := combined.Wrap(&cipherBuf)
	_, _ = wc.Write([]byte("data"))
	_ = wc.Close()

	eph.Wipe()

	drDec, err := NewDecryptorFromKeys(drPrivKey)
	if err != nil {
		t.Fatal(err)
	}
	r, err := drDec.Wrap(bytes.NewReader(cipherBuf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "data" {
		t.Errorf("DR decrypt after wipe: %q", got)
	}
}
