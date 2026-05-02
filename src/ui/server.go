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
	"strconv"
	"strings"
	"time"

	"backup-operator/dumper"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Config carries everything the server needs to render itself.
type Config struct {
	Addr              string // ":8081" by default — kept off the metrics port to keep concerns separate
	Namespace         string
	Client            client.Client
	Logger            logr.Logger
	SettingsConfigMap string // name of the ConfigMap for runtime-configurable settings (empty = disabled)
}

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
	return &Server{
		cfg:  cfg,
		tpl:  tpl,
		data: newK8sData(cfg.Client, cfg.Namespace, cfg.Logger.WithName("data")),
		sse:  newSSEBroker(),
	}, nil
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

	// Read-only JSON API
	mux.HandleFunc("/api/targets", s.handleAPITargets)
	mux.HandleFunc("/api/targets/", s.handleAPITargetRuns)
	mux.HandleFunc("/api/destinations", s.routeDestinationsList)
	mux.HandleFunc("/api/jobs", s.handleAPIJobs)

	// CRUD API
	mux.HandleFunc("/api/sources", s.handleAPICreateSource)
	mux.HandleFunc("/api/sources/", s.routeSourceByMethod)
	mux.HandleFunc("/api/destinations/", s.routeDestinationByMethod)

	// Manual trigger
	mux.HandleFunc("/api/trigger/", s.handleAPITriggerBackup)

	// Settings API
	mux.HandleFunc("/api/settings", s.routeSettings)
	mux.HandleFunc("/api/settings/export", s.handleSettingsExport)

	// Multi-storage enterprise endpoints
	mux.HandleFunc("/api/destination-health", s.handleAPIDestinationHealth)
	mux.HandleFunc("/api/destination-stats", s.handleAPIDestinationStats)
	mux.HandleFunc("/api/consistency-check", s.handleAPIConsistencyCheck)

	// SSE live updates
	mux.HandleFunc("/api/events", s.handleSSE)

	// Downloads
	mux.HandleFunc("/download/", s.handleDownload)

	// Health
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	// SPA catch-all: serve index.html for any unmatched path
	mux.HandleFunc("/", s.handleSPA)

	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
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

func noCacheMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) routeSourceByMethod(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleAPIGetSource(w, r)
	case http.MethodPut:
		s.handleAPIUpdateSource(w, r)
	case http.MethodDelete:
		s.handleAPIDeleteSource(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "method not allowed"})
	}
}

func (s *Server) routeDestinationsList(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleAPIListDestinations(w, r)
	case http.MethodPost:
		s.handleAPICreateDestination(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "method not allowed"})
	}
}

func (s *Server) routeSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetSettings(w, r)
	case http.MethodPut:
		s.handleUpdateSettings(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "method not allowed"})
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
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "method not allowed"})
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
