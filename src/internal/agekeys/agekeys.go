// Package agekeys holds the naming helpers shared by everything that creates
// or displays BackupRecipient objects: the operator's bootstrap seeding, the
// UI's Age-Keys page and the legacy migration tool. One hash, one naming
// rule, so the same public key always lands on the same object name.
package agekeys

import (
	"crypto/sha256"
	"encoding/hex"
)

// Hash is the 12-hex-char SHA256 prefix used as a display fingerprint and as
// the stable suffix in recipient object names. 48 bits is overkill for the
// handful of recipients a cluster has, and cheap.
func Hash(publicKey string) string {
	sum := sha256.Sum256([]byte(publicKey))
	return hex.EncodeToString(sum[:])[:12]
}

// ObjectName derives a deterministic BackupRecipient name from the public
// key, so re-adding the same key is idempotent (Create surfaces 409 Conflict
// instead of duplicating).
func ObjectName(publicKey string) string {
	return "backup-recipient-" + Hash(publicKey)
}
