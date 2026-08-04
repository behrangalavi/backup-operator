package dumper

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// HashSchema returns a stable SHA256 fingerprint of a set of schema
// identifiers (collection names, redis DB indexes, …). The seed is sorted so
// the hash is order-independent, and each element is newline-terminated so
// "ab"+"c" and "a"+"bc" don't collide. Shared by the mongo and redis dumpers,
// which have no column-level schema to fingerprint — the sorted identifier set
// is the closest analogue, and a change in it surfaces as a schema change.
func HashSchema(seed []string) string {
	sort.Strings(seed)
	h := sha256.New()
	for _, s := range seed {
		h.Write([]byte(s))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}
