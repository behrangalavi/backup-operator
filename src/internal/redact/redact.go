// Package redact scrubs credential material out of error strings before they
// reach logs, Kubernetes status fields, Events or HTTP responses. Shared by
// the UI's diagnostic endpoints and the controllers' status writers so both
// surfaces apply the same rules.
package redact

import "regexp"

var (
	// scheme://user:pass@host  → keep scheme + host, drop userinfo
	reURICredentials = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@\s]*@`)
	// password=secret / pwd=… / auth=… / token=… / access-key=… (any case)
	reKVCredentials = regexp.MustCompile(`(?i)\b(pass(word|wd)?|pwd|auth|token|secret[-_]?(access[-_]?)?key|access[-_]?key([-_]?id)?)\s*[=:]\s*\S+`)
)

// StorageError scrubs credential material from a storage backend error while
// preserving the protocol-level diagnostic that makes it useful ("550
// Permission denied", "no space left on device", "connection refused").
// Returns "" for nil.
func StorageError(err error) string {
	if err == nil {
		return ""
	}
	return String(err.Error())
}

// String applies the same scrubbing to an arbitrary message.
func String(msg string) string {
	msg = reURICredentials.ReplaceAllString(msg, "${1}***@")
	msg = reKVCredentials.ReplaceAllString(msg, "${1}=***")
	return msg
}
