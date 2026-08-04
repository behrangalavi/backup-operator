package crypto

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"filippo.io/age"
)

// EphemeralIdentity holds an X25519 keypair generated in-process for a
// single restore-verifier run. The private half is used only inside the
// worker pod that produced it; once that pod terminates, the key is gone.
//
// The public half is added to the encryptor's recipient list so the
// resulting dump can be decrypted by either the long-lived DR recipient
// (offline) OR this single ephemeral identity (in-memory). After the
// verifier returns, callers MUST call Wipe() to drop the identity reference
// as defence-in-depth — Go cannot guarantee no copies remain in the heap,
// but we release it promptly rather than holding it for the pod's lifetime.
type EphemeralIdentity struct {
	identity *age.X25519Identity
}

// GenerateEphemeralIdentity creates a fresh X25519 keypair in process memory.
// Returns an opaque handle; callers extract the public recipient line via
// PublicLine() and decrypt with Decryptor() at verifier time.
func GenerateEphemeralIdentity() (*EphemeralIdentity, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("generate ephemeral X25519 identity: %w", err)
	}
	return &EphemeralIdentity{identity: id}, nil
}

// PublicLine returns the public recipient as the standard age-formatted
// "age1..." string suitable for ParseX25519Recipient.
func (e *EphemeralIdentity) PublicLine() string {
	if e == nil || e.identity == nil {
		return ""
	}
	return e.identity.Recipient().String()
}

// RecipientFingerprint returns a short, stable identifier for the ephemeral
// public key. Suitable for logging and persisting into meta.json so an
// auditor can correlate a verifier-run with the key it used. Truncated
// SHA256 of the recipient line; not cryptographically meaningful, just a
// label.
func (e *EphemeralIdentity) RecipientFingerprint() string {
	pub := e.PublicLine()
	if pub == "" {
		return ""
	}
	h := sha256.Sum256([]byte(pub))
	return hex.EncodeToString(h[:8])
}

// Decryptor returns a Decryptor scoped to this ephemeral identity only.
// Use this in the verifier to decrypt the local temp file the worker just
// wrote. Returns an error if the identity has been wiped.
func (e *EphemeralIdentity) Decryptor() (Decryptor, error) {
	if e == nil || e.identity == nil {
		return nil, fmt.Errorf("ephemeral identity unavailable (wiped or nil)")
	}
	return &ageDecryptor{identities: []age.Identity{e.identity}}, nil
}

// Wipe drops the reference to the underlying identity so it becomes
// ineligible for use and the GC can reclaim it. Best-effort defence-in-depth:
// Go does not guarantee no copies of the secret material exist elsewhere in
// the heap (and age keeps the scalar internally), but holding the private key
// past the verifier phase serves no purpose, so we release it promptly.
func (e *EphemeralIdentity) Wipe() {
	if e == nil {
		return
	}
	e.identity = nil
}

// NewEncryptorWithExtraRecipient returns a new Encryptor whose recipient
// list is the existing encryptor's recipients PLUS extraPublic. The base
// encryptor is unchanged (immutable) — the per-run encryptor is a sibling.
//
// extraPublic must be a single age recipient line (no newline-separated
// multi-recipient input). Empty extraPublic returns the base encryptor as
// a no-op for callers that conditionally enable verification.
func NewEncryptorWithExtraRecipient(base Encryptor, extraPublic string) (Encryptor, error) {
	extraPublic = strings.TrimSpace(extraPublic)
	if extraPublic == "" {
		return base, nil
	}
	r, err := age.ParseX25519Recipient(extraPublic)
	if err != nil {
		return nil, fmt.Errorf("parse extra recipient: %w", err)
	}
	baseAge, ok := base.(*ageEncryptor)
	if !ok {
		return nil, fmt.Errorf("base encryptor is not an age encryptor (got %T)", base)
	}
	merged := make([]age.Recipient, 0, len(baseAge.recipients)+1)
	merged = append(merged, baseAge.recipients...)
	merged = append(merged, r)
	return &ageEncryptor{recipients: merged}, nil
}

