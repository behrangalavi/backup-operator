package ui

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync"
)

// cacheableMaxBytes caps the body size we will buffer for ETag computation.
// Larger responses skip the buffer (ETag and gzip are then no-ops). At the
// scale of this UI (a few hundred sources × small JSON each), 4 MiB is two
// orders of magnitude over expectation, so anything above is treated as a
// pathological response not worth memory pressure.
const cacheableMaxBytes = 4 << 20

// cachedJSON wraps a handler that writes a JSON body. It buffers the body,
// computes a SHA-256-prefix ETag, and serves 304 when the client's
// If-None-Match matches. The response body is gzipped if Accept-Encoding
// allows. Cache-Control is private+short so a misbehaving shared cache
// cannot serve stale data to a different user/tenant, while a single
// browser still saves bandwidth on rapid SSE-driven re-fetches.
//
// Use only on read-only endpoints whose response is purely a function of
// current cluster state. Endpoints that stream (SSE) must not be wrapped.
func cachedJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		buf := bufPool.Get().(*bytes.Buffer)
		buf.Reset()
		defer bufPool.Put(buf)

		rec := &bufferedRecorder{
			ResponseWriter: w,
			buf:            buf,
			max:            cacheableMaxBytes,
			header:         http.Header{},
		}
		next.ServeHTTP(rec, r)

		// Pass through unbuffered responses (oversized or already-streamed)
		// — there is nothing left to send.
		if rec.passthrough {
			return
		}

		// Errors and redirects skip caching; the body is small but its
		// content (and lifetime) makes ETag misleading.
		if rec.status >= 400 || (rec.status >= 300 && rec.status < 400) {
			copyHeaders(w.Header(), rec.header)
			if rec.status != 0 {
				w.WriteHeader(rec.status)
			}
			_, _ = w.Write(rec.buf.Bytes())
			return
		}

		etag := strongETag(rec.buf.Bytes())
		copyHeaders(w.Header(), rec.header)
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "private, max-age=5, must-revalidate")
		w.Header().Add("Vary", "Accept-Encoding")

		if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		body := rec.buf.Bytes()
		if shouldGzip(r, body) {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Del("Content-Length") // unknown after compression
			if rec.status != 0 {
				w.WriteHeader(rec.status)
			}
			gz := gzipPool.Get().(*gzip.Writer)
			gz.Reset(w)
			_, _ = gz.Write(body)
			_ = gz.Close()
			gzipPool.Put(gz)
			return
		}
		if rec.status != 0 {
			w.WriteHeader(rec.status)
		}
		_, _ = w.Write(body)
	})
}

// strongETag is a strong validator (no W/ prefix). The 16 hex chars give
// 64 bits of collision space — comfortably more than the number of
// distinct responses this UI ever produces.
func strongETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// etagMatches handles comma-separated If-None-Match lists ("etag1", "etag2")
// per RFC 7232 §3.2 and the special "*" wildcard. Trims whitespace and
// quotes for tolerance against ad-hoc clients.
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		c := strings.TrimSpace(candidate)
		if c == "*" || c == etag {
			return true
		}
	}
	return false
}

// shouldGzip returns true when the client advertises gzip and the body is
// large enough that compression overhead is worth it. Below ~1 KB the
// gzip headers and CPU cost typically inflate the response.
func shouldGzip(r *http.Request, body []byte) bool {
	if len(body) < 1024 {
		return false
	}
	enc := r.Header.Get("Accept-Encoding")
	for _, part := range strings.Split(enc, ",") {
		if strings.TrimSpace(strings.SplitN(part, ";", 2)[0]) == "gzip" {
			return true
		}
	}
	return false
}

func copyHeaders(dst, src http.Header) {
	for k, v := range src {
		dst[k] = v
	}
}

// bufferedRecorder captures status + body so the surrounding middleware
// can compute an ETag, optionally gzip, and emit Cache-Control before
// flushing to the real ResponseWriter.
type bufferedRecorder struct {
	http.ResponseWriter
	buf         *bytes.Buffer
	max         int
	header      http.Header
	status      int
	passthrough bool // true once we've spilled to the real writer
}

func (b *bufferedRecorder) Header() http.Header {
	if b.passthrough {
		return b.ResponseWriter.Header()
	}
	return b.header
}

func (b *bufferedRecorder) WriteHeader(code int) {
	b.status = code
	if b.passthrough {
		b.ResponseWriter.WriteHeader(code)
	}
}

func (b *bufferedRecorder) Write(p []byte) (int, error) {
	if b.passthrough {
		return b.ResponseWriter.Write(p)
	}
	if b.buf.Len()+len(p) > b.max {
		// Spill: send headers and accumulated body to the real writer,
		// then mark passthrough so subsequent writes go straight through.
		copyHeaders(b.ResponseWriter.Header(), b.header)
		if b.status != 0 {
			b.ResponseWriter.WriteHeader(b.status)
		}
		if _, err := b.ResponseWriter.Write(b.buf.Bytes()); err != nil {
			return 0, err
		}
		b.buf.Reset()
		b.passthrough = true
		return b.ResponseWriter.Write(p)
	}
	return b.buf.Write(p)
}

// Implement http.Flusher passthrough so handlers that flush mid-write
// (none in this codebase today, but be safe) trigger the spill rather
// than buffering forever. Without this, a flushing handler would hang.
func (b *bufferedRecorder) Flush() {
	if !b.passthrough && b.buf.Len() > 0 {
		copyHeaders(b.ResponseWriter.Header(), b.header)
		if b.status != 0 {
			b.ResponseWriter.WriteHeader(b.status)
		}
		_, _ = b.ResponseWriter.Write(b.buf.Bytes())
		b.buf.Reset()
		b.passthrough = true
	}
	if f, ok := b.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Pools amortise the per-request allocation for the buffer and the gzip
// writer. ETag-cached endpoints fire on every SSE-driven UI refresh; the
// pool turns those into stable steady-state allocations.
var (
	bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}
	gzipPool = sync.Pool{New: func() any {
		// Default compression — speed/size tradeoff is fine for JSON of
		// the sizes we serve. If a future endpoint serves much larger
		// bodies, evaluate gzip.BestSpeed.
		w, _ := gzip.NewWriterLevel(io.Discard, gzip.DefaultCompression)
		return w
	}}
)
