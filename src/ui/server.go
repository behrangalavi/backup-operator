// Package ui exposes an HTML/JSON dashboard and CRUD API for the
// backup-operator. It runs as an in-process HTTP server alongside
// the controller manager, providing:
//
//   - Read-only dashboard for backup targets and run history
//   - CRUD endpoints for source and destination Secrets
//   - Manual backup trigger via Job creation
//   - Server-Sent Events (SSE) for live status updates
package ui

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"backup-operator/dumper"
	"backup-operator/internal/alerts"
	"backup-operator/internal/secrets"
	"backup-operator/metrics"
	"backup-operator/storage"
	storageFactory "backup-operator/storage/factory"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// StoragePool is the surface the UI needs from the process-wide
// controllers.StoragePool. Defined here as an interface so the UI
// package doesn't take a hard dependency on controllers — the
// operator's reconcile loops should stay a leaf, not a UI dep.
// The concrete *controllers.StoragePool satisfies this structurally.
type StoragePool interface {
	Get(d *secrets.Destination) (storage.Storage, error)
}

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// Config carries everything the server needs to render itself.
type Config struct {
	Addr              string // ":8081" by default — kept off the metrics port to keep concerns separate
	Namespace         string
	Client            client.Client
	APIReader         client.Reader // uncached reads (events list, where caching would force a watch on a noisy resource)
	Logger            logr.Logger
	SettingsConfigMap string // name of the ConfigMap for runtime-configurable settings (empty = disabled)
	AgeSecretName     string // name of the Secret holding AGE_PUBLIC_KEYS (empty = key listing/mutation disabled)
	ReadOnly          bool   // when true, all mutation endpoints return 403
	AllowKeyMutation  bool   // when true, age-key add/remove endpoints are exposed (read-only listing always available)
	MaxBodyBytes      int64  // request body cap; 0 = use defaultMaxBodyBytes
	MaxSSEClients     int    // concurrent SSE subscribers; 0 = use defaultMaxSSEClients

	// AlertsProvider supplies /api/alerts. When PrometheusURL is configured
	// in main.go we install a PrometheusProvider with a LocalProvider
	// fallback; otherwise just LocalProvider. Optional — when nil, the
	// /api/alerts endpoint returns 503 with a helpful message instead of
	// pretending to know.
	AlertsProvider alerts.Provider

	// PrometheusURL is the configured Prometheus endpoint. Stored here so
	// the /api/alerts/status endpoint can report connectivity. Empty means
	// "not configured — using local heuristic only."
	PrometheusURL string

	// AlertmanagerURL is used for "open in Alertmanager" links in the UI,
	// for the /api/alerts/status connectivity check (GET /api/v2/status),
	// and for the /api/alerts/test endpoint (POST /api/v2/alerts).
	AlertmanagerURL string

	// WorkerServiceAccount is the name of the worker SA in Namespace.
	// Used by /api/cluster/capabilities to run a SubjectAccessReview
	// against pods/create — the gate for Phase-2 restore-verification
	// modes. Empty disables the capability check (the endpoint then
	// reports "configuration unknown" rather than guessing).
	WorkerServiceAccount string

	// Pool is the process-wide storage client cache, shared with the
	// MetricsRefresher and StorageScrubber controllers. UI live-probe
	// endpoints (test-connection, destination-stats, destination-health,
	// consistency-check, fleet heatmap, run download) get cached SFTP/
	// FTPS/S3 clients instead of building fresh ones on every request
	// — which was the root cause of QNAP's Network Access Protection
	// blocking the operator IP after a few minutes of dashboard use.
	// Optional: when nil, the UI falls back to per-call construction.
	Pool StoragePool
}

// Conservative defaults sized for an enterprise deployment with thousands of
// targets. Body limit is large enough for any realistic source/destination
// payload (SSH keys, small known-hosts blobs) but blocks GB-scale bodies that
// would OOM the operator. SSE cap protects against client-side hoarding.
const (
	defaultMaxBodyBytes  = 1 << 20 // 1 MiB
	defaultMaxSSEClients = 256
)

// Server is constructed once at process start and run by Start.
type Server struct {
	cfg  Config
	tpl  *template.Template
	data dataSource
	sse  *sseBroker
}

func New(cfg Config) (*Server, error) {
	tpl, err := template.New("ui").Funcs(funcMap()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	if cfg.MaxSSEClients <= 0 {
		cfg.MaxSSEClients = defaultMaxSSEClients
	}
	broker := newSSEBroker()
	broker.maxClients = cfg.MaxSSEClients
	data := newK8sData(cfg.Client, cfg.Namespace, cfg.Logger.WithName("data"))
	data.pool = cfg.Pool
	s := &Server{
		cfg:  cfg,
		tpl:  tpl,
		data: data,
		sse:  broker,
	}
	// When a background storage probe finishes and refreshes the cache,
	// emit a "refresh" SSE so the frontend can repaint with fresh data.
	// The dashboard render path returns immediately with stale cache; this
	// event closes the loop. Wired on the concrete *k8sData rather than
	// the dataSource interface — onRefresh is an internal coupling
	// between cache and broker, not part of the data API.
	data.onRefresh = func() {
		s.broadcast(sseEvent{Type: "refresh", Data: "targets"})
	}
	return s, nil
}

// Start blocks until ctx is cancelled, after which the HTTP listener is
// shut down with a short grace period.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	// SPA frontend — serves index.html for the root, static assets for /static/
	// no-cache forces the browser to revalidate on every load so new deploys
	// are picked up immediately (embedded files change on rebuild).
	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("/static/", noCacheMiddleware(http.StripPrefix("/static/", http.FileServer(http.FS(staticSub)))))

	// Legacy template routes (kept for backward compatibility)
	mux.HandleFunc("/legacy", s.handleIndex)
	mux.HandleFunc("/legacy/target/", s.handleTarget)

	// Read-only JSON API. The high-volume read endpoints get ETag + gzip +
	// short Cache-Control via cachedJSON; SSE-driven UIs poll these on
	// every refresh tick so cache hits matter. /api/destinations is also
	// a write entrypoint (POST), but cachedJSON only acts on GET/HEAD so
	// the route can stay shared.
	mux.Handle("/api/targets", cachedJSON(http.HandlerFunc(s.handleAPITargets)))
	mux.HandleFunc("/api/targets/", s.handleAPITargetRuns)
	mux.Handle("/api/destinations", cachedJSON(http.HandlerFunc(s.routeDestinationsList)))
	mux.Handle("/api/jobs", cachedJSON(http.HandlerFunc(s.handleAPIJobs)))

	// CRUD API
	mux.HandleFunc("/api/sources", s.handleAPICreateSource)
	mux.HandleFunc("/api/sources/", s.routeSourceByMethod)
	mux.HandleFunc("/api/destinations/", s.routeDestinationByMethod)

	// Manual trigger
	mux.HandleFunc("/api/trigger/", s.handleAPITriggerBackup)

	// Settings API
	mux.HandleFunc("/api/settings", s.routeSettings)
	mux.HandleFunc("/api/settings/export", s.handleSettingsExport)

	// Age recipient (public key) management — listing always available;
	// add/remove gated behind UI_READ_ONLY=false + UI_ALLOW_KEY_MUTATION=true.
	mux.HandleFunc("/api/age-keys", s.routeAgeKeys)
	mux.HandleFunc("/api/age-keys/", s.routeAgeKeyByRecipient)

	// Audit log — reads Events emitted by our own components (worker,
	// reconciler, UI). Read-only.
	mux.HandleFunc("/api/audit-log", s.handleAuditLog)

	// Multi-storage enterprise endpoints
	mux.HandleFunc("/api/destination-health", s.handleAPIDestinationHealth)
	mux.HandleFunc("/api/destination-stats", s.handleAPIDestinationStats)
	mux.HandleFunc("/api/consistency-check", s.handleAPIConsistencyCheck)

	// Dashboard fleet heatmap — per-target, per-day status grid
	mux.Handle("/api/dashboard/heatmap", cachedJSON(http.HandlerFunc(s.handleAPIFleetHeatmap)))

	// SSE live updates
	mux.HandleFunc("/api/events", s.handleSSE)

	// Alerts surface (Prometheus or local heuristic)
	mux.HandleFunc("/api/alerts", s.handleAlerts)
	mux.HandleFunc("/api/alerts/status", s.handleAlertsStatus)
	mux.HandleFunc("/api/alerts/test", s.handleAlertsTest)

	mux.HandleFunc("/api/cluster/capabilities", s.handleAPIClusterCapabilities)

	// Downloads
	mux.HandleFunc("/download/", s.handleDownload)

	// Health
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	// SPA catch-all: serve index.html for any unmatched path
	mux.HandleFunc("/", s.handleSPA)

	srv := &http.Server{
		Addr: s.cfg.Addr,
		// Cap request bodies globally. None of our endpoints legitimately
		// accept large uploads — the worst case is a few KiB of JSON or an
		// SSH key blob. Without this an unauthenticated POST of a multi-GB
		// body OOMs the operator.
		Handler:           latencyMiddleware(readOnlyMiddleware(s.cfg.ReadOnly, csrfMiddleware(limitBodyMiddleware(s.cfg.MaxBodyBytes, mux)))),
		ReadHeaderTimeout: 5 * time.Second,
		// MaxHeaderBytes defaults to 1MB which is fine; lower would require
		// careful audit of cookies/auth-proxy headers downstream users add.
	}

	go s.periodicRefresh(ctx)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	s.cfg.Logger.Info("ui server listening", "addr", s.cfg.Addr, "namespace", s.cfg.Namespace)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// storageFor returns a storage.Storage for the destination, preferring
// the shared pool (clients amortised across MetricsRefresher, scrubber,
// and every UI handler) and falling back to a fresh build when no pool
// is wired. Use this in every UI handler that touches a destination —
// direct storageFactory.NewStorage calls open a fresh client per
// request and were the trigger for QNAP NAP-blocking the operator's IP.
func (s *Server) storageFor(d *secrets.Destination, logName string) (storage.Storage, error) {
	return storageForPool(s.cfg.Pool, d, s.cfg.Logger.WithName(logName))
}

// storageForPool is the single pool-or-fallback implementation behind both
// Server.storageFor and k8sData.storageFor. A nil pool (tests, unwired data
// layer) builds a fresh client; otherwise the shared pool amortises clients
// across handlers and avoids the per-request handshake storm that tripped
// QNAP's NAP IP-block (§18 ADR).
func storageForPool(pool StoragePool, d *secrets.Destination, log logr.Logger) (storage.Storage, error) {
	// SSRF guard: refuse to build a client (and therefore refuse the dial that
	// follows) for a destination whose user-supplied host points at a blocked
	// range — cloud metadata / link-local / loopback. This is the single choke
	// point every UI storage path routes through, and the UI has no auth, so
	// the check belongs here rather than in each diagnostic handler.
	if err := checkDestinationEgress(d); err != nil {
		return nil, err
	}
	if pool != nil {
		return pool.Get(d)
	}
	return storageFactory.NewStorage(d.StorageType, d.Name, d.Data, log)
}

func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	// For API routes that fell through, return 404
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		renderError(w, http.StatusInternalServerError, "SPA not found")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// csrfMiddleware rejects browser-driven cross-origin state changes. The UI
// has no built-in auth (§3.1), so when it sits behind a cookie-based auth
// proxy a malicious page could otherwise drive any mutating endpoint via the
// victim's browser. We gate on Sec-Fetch-Site (set by all modern browsers,
// computed client-side so it survives reverse proxies) and fall back to an
// Origin/Host comparison for older browsers. Non-browser clients (curl, the
// restore CLI, monitoring) send neither header and pass through unchanged —
// this narrows the browser attack surface, it does not replace the auth proxy.
func csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		switch r.Header.Get("Sec-Fetch-Site") {
		case "cross-site", "same-site":
			writeError(w, http.StatusForbidden, codeForbidden, "cross-origin request rejected")
			return
		}
		// Defence in depth for browsers that send Origin but not
		// Sec-Fetch-Site: the Origin host must match the request host.
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || !originMatchesHost(u.Host, r) {
				writeError(w, http.StatusForbidden, codeForbidden, "cross-origin request rejected")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// originMatchesHost reports whether the Origin header's host belongs to the
// same site the request was addressed to. X-Forwarded-Host is honoured for
// the documented reverse-proxy deployment; it is spoofable, but an attacker
// who can set it can already reach the operator directly, so it adds no risk.
func originMatchesHost(originHost string, r *http.Request) bool {
	if originHost == "" {
		return false
	}
	if strings.EqualFold(originHost, r.Host) {
		return true
	}
	if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" && strings.EqualFold(originHost, xfh) {
		return true
	}
	return false
}

// readOnlyMiddleware enforces UI_READ_ONLY globally: when enabled, every
// non-safe method (anything but GET/HEAD/OPTIONS) is rejected with 403 before
// it reaches a handler. Applied globally rather than per-route — like CSRF and
// the body cap — so a mutating endpoint added later cannot silently escape the
// gate by forgetting an `if s.cfg.ReadOnly` check (the exact hole this closes:
// only the age-key and alerts-test handlers checked the flag, so source/dest
// CRUD, trigger, suspend, test-connection and settings were mutable despite
// UI_READ_ONLY=true). Every mutation here uses POST/PUT/DELETE, so method-based
// gating covers them all, matching csrfMiddleware's model.
func readOnlyMiddleware(readOnly bool, next http.Handler) http.Handler {
	if !readOnly {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, http.StatusForbidden, codeForbidden, "read-only mode: mutations are disabled")
	})
}

func noCacheMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// limitBodyMiddleware wraps r.Body with http.MaxBytesReader so any handler
// that decodes the body sees an EOF after the cap is hit. Requests with no
// body (GET/SSE/downloads) are unaffected. We deliberately apply this
// globally rather than per-route so a new mutating endpoint added later
// inherits the protection without anyone having to remember.
func limitBodyMiddleware(max int64, next http.Handler) http.Handler {
	if max <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = http.MaxBytesReader(w, r.Body, max)
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.code = code
	sr.ResponseWriter.WriteHeader(code)
}

func latencyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip static assets and SSE — they are long-lived or high-volume
		// and would pollute the histogram with noise.
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/api/events" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		next.ServeHTTP(rec, r)
		p := normalizeMetricPath(r.URL.Path)
		metrics.HTTPRequestDuration.WithLabelValues(r.Method, p, strconv.Itoa(rec.code)).Observe(time.Since(start).Seconds())
	})
}

// metricPathPrefixes are parametric routes whose trailing resource id must be
// stripped so /api/sources/foo and /api/sources/bar collapse to one label.
var metricPathPrefixes = []string{
	"/api/sources/", "/api/destinations/", "/api/targets/", "/api/trigger/",
	"/api/age-keys/", "/download/", "/legacy/target/",
}

// metricKnownPaths is the closed set of fixed routes the mux registers. Only
// these pass through as a `path` label verbatim; see normalizeMetricPath.
var metricKnownPaths = map[string]bool{
	"/": true, "/legacy": true, "/healthz": true,
	"/api/targets": true, "/api/destinations": true, "/api/jobs": true,
	"/api/sources": true, "/api/settings": true, "/api/settings/export": true,
	"/api/age-keys": true, "/api/audit-log": true,
	"/api/destination-health": true, "/api/destination-stats": true,
	"/api/consistency-check": true, "/api/dashboard/heatmap": true,
	"/api/alerts": true, "/api/alerts/status": true, "/api/alerts/test": true,
	"/api/cluster/capabilities": true,
}

// normalizeMetricPath bounds the cardinality of the `path` label on the
// request-duration histogram. Parametric routes collapse to their prefix;
// the fixed route set passes through; EVERYTHING else becomes "other".
//
// Without the "other" fallback the SPA catch-all ("/" handler serves any
// path) and unknown /api/* 404s would each mint a fresh histogram series —
// 11 buckets apiece, retained for the pod's lifetime — so an unauthenticated
// scanner walking /wp-admin, /.env, /a/b/<random> is an unbounded-memory /
// scrape-bloat DoS on the operator's registry. The UI has no auth by design
// (§3.1), so this must be closed at the metric layer.
func normalizeMetricPath(path string) string {
	for _, prefix := range metricPathPrefixes {
		if strings.HasPrefix(path, prefix) {
			return prefix
		}
	}
	if metricKnownPaths[path] {
		return path
	}
	return "other"
}

func (s *Server) routeSourceByMethod(w http.ResponseWriter, r *http.Request) {
	rest := trimPrefixPath(r.URL.Path, "/api/sources/")
	if strings.HasSuffix(rest, "/suspend") {
		s.handleAPISuspendSource(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleAPIGetSource(w, r)
	case http.MethodPut:
		s.handleAPIUpdateSource(w, r)
	case http.MethodDelete:
		s.handleAPIDeleteSource(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) routeDestinationsList(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleAPIListDestinations(w, r)
	case http.MethodPost:
		s.handleAPICreateDestination(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) routeSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetSettings(w, r)
	case http.MethodPut:
		s.handleUpdateSettings(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) routeDestinationByMethod(w http.ResponseWriter, r *http.Request) {
	rest := trimPrefixPath(r.URL.Path, "/api/destinations/")
	if strings.HasSuffix(rest, "/test") {
		s.handleAPITestDestination(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleAPIGetDestination(w, r)
	case http.MethodPut:
		s.handleAPIUpdateDestination(w, r)
	case http.MethodDelete:
		s.handleAPIDeleteDestination(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed")
	}
}

func funcMap() template.FuncMap {
	return template.FuncMap{
		"humanBytes":    humanBytes,
		"percentChange": percentChange,
		"totalRows":     totalRows,
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func percentChange(ratio float64) string {
	if ratio == 0 {
		return "—"
	}
	delta := (ratio - 1) * 100
	sign := ""
	if delta > 0 {
		sign = "+"
	}
	return fmt.Sprintf("%s%.1f%%", sign, delta)
}

func totalRows(s *dumper.Stats) string {
	if s == nil {
		return "—"
	}
	var sum int64
	for _, t := range s.Tables {
		sum += t.RowCount
	}
	return strconv.FormatInt(sum, 10)
}

func renderError(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	_, _ = w.Write([]byte(msg))
}

func trimPrefixPath(p, prefix string) string {
	return strings.TrimPrefix(p, prefix)
}
