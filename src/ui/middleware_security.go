package ui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/go-logr/logr"
)

// contentSecurityPolicy is deliberately strict: the SPA has no inline event
// handlers (every control is wired through data-action delegation — see
// core.js) and loads nothing from third parties, so script-src is 'self'
// only. Inline *styles* (style="…" attributes and the legacy templates'
// <style> block) are everywhere, hence 'unsafe-inline' for style-src only —
// a CSS injection cannot run code, and the SPA escapes attribute values.
// frame-ancestors 'none' plus X-Frame-Options: DENY blocks clickjacking of a
// dashboard that has no built-in auth (§3.1).
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; connect-src 'self'; font-src 'self'; object-src 'none'; " +
	"base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

// securityHeadersMiddleware stamps the response-hardening headers on every
// response, API and SPA alike. Set before the handler runs so handlers that
// write their own headers (SSE, downloads) still carry them.
func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		next.ServeHTTP(w, r)
	})
}

type requestCtxKey int

const (
	ctxKeyRequestID requestCtxKey = iota
	ctxKeyCaller
)

// identityHeaders are the headers the common auth proxies put the
// authenticated principal in, in the order we trust them. oauth2-proxy sets
// X-Forwarded-User / X-Forwarded-Email (and X-Auth-Request-* when
// --set-xauthrequest is on); Pomerium and Traefik ForwardAuth use the
// X-Forwarded-* pair. The first non-empty one wins.
var identityHeaders = []string{
	"X-Forwarded-User", "X-Forwarded-Email", "X-Auth-Request-User", "X-Auth-Request-Email", "X-Forwarded-Preferred-Username",
}

const maxIdentityLen = 128

// callerIdentity extracts the auth-proxy principal from the request. The
// value is attacker-influenced when no proxy is in front (anyone can send
// the header), so it is sanitised to printable characters and length-capped
// before it is written into Events and logs — it is an audit hint, never an
// authorisation input.
func callerIdentity(r *http.Request) string {
	for _, h := range identityHeaders {
		if v := sanitizeIdentity(r.Header.Get(h)); v != "" {
			return v
		}
	}
	return ""
}

func sanitizeIdentity(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	var b strings.Builder
	for _, rn := range v {
		if rn < 0x20 || rn == 0x7f || !unicode.IsPrint(rn) {
			continue
		}
		b.WriteRune(rn)
		if b.Len() >= maxIdentityLen {
			break
		}
	}
	return b.String()
}

// sanitizeRequestID accepts a client-supplied X-Request-Id only when it is a
// short, boring token; anything else is replaced so log injection via the
// header is impossible.
func sanitizeRequestID(v string) string {
	if v == "" || len(v) > 64 {
		return ""
	}
	for _, rn := range v {
		ok := (rn >= 'a' && rn <= 'z') || (rn >= 'A' && rn <= 'Z') || (rn >= '0' && rn <= '9') || rn == '-' || rn == '_' || rn == '.'
		if !ok {
			return ""
		}
	}
	return v
}

func newRequestID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "r-" + hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))[:16]
	}
	return hex.EncodeToString(buf[:])
}

// requestIDFromContext returns the request id stamped by requestContextMiddleware.
func requestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyRequestID).(string)
	return v
}

// callerFromContext returns the sanitised auth-proxy identity, or "".
func callerFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyCaller).(string)
	return v
}

// requestContextMiddleware is the outermost middleware: it assigns (or
// passes through) a request id, extracts the caller identity, puts both into
// the request context for handlers and the Event emitter, echoes the id in
// X-Request-Id, and writes one structured access-log line per request at
// V(1). Static assets and the SSE stream are skipped — the former are noise,
// the latter never "finishes".
func requestContextMiddleware(log logr.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get("X-Request-Id"))
		if id == "" {
			id = newRequestID()
		}
		caller := callerIdentity(r)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		ctx = context.WithValue(ctx, ctxKeyCaller, caller)
		w.Header().Set("X-Request-Id", id)
		r = r.WithContext(ctx)

		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/api/events" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		next.ServeHTTP(rec, r)
		fields := []any{
			"method", r.Method, "path", r.URL.Path, "status", rec.code,
			"duration_ms", time.Since(start).Milliseconds(), "request_id", id,
		}
		if caller != "" {
			fields = append(fields, "caller", caller)
		}
		log.V(1).Info("http request", fields...)
	})
}

// auditSuffix renders the caller/request-id trailer appended to UI-emitted
// Event messages so `kubectl describe` answers "who did this, from which
// request" without a separate access-log lookup.
func auditSuffix(ctx context.Context) string {
	caller := callerFromContext(ctx)
	id := requestIDFromContext(ctx)
	switch {
	case caller != "" && id != "":
		return " (by " + caller + ", request " + id + ")"
	case caller != "":
		return " (by " + caller + ")"
	case id != "":
		return " (request " + id + ")"
	}
	return ""
}

// noDirListing wraps a file server so directory URLs (trailing slash) are
// 404 instead of an auto-generated index of the embedded assets.
func noDirListing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") || r.URL.Path == "" {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
