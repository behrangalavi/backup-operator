// Package ui exposes a minimal read-only HTML/JSON dashboard for the
// backup-operator operator. It runs as an in-process HTTP server alongside
// the controller manager and answers two questions a human operator asks
// in practice:
//
//   - What targets does this namespace back up?
//   - What does the run history of a given target look like?
//
// It does not write anything to the cluster, hold encryption keys, or
// trigger backups. Anything that would do so belongs in a separate v2
// (or in the existing CLI surface).
package ui

import (
	"context"
	"embed"
	"fmt"
	"html/template"
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

// Config carries everything the server needs to render itself.
type Config struct {
	Addr      string // ":8081" by default — kept off the metrics port to keep concerns separate
	Namespace string
	Client    client.Client
	Logger    logr.Logger
}

// Server is constructed once at process start and run by Start.
type Server struct {
	cfg  Config
	tpl  *template.Template
	data dataSource
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
	}, nil
}

// Start blocks until ctx is cancelled, after which the HTTP listener is
// shut down with a short grace period.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/target/", s.handleTarget)
	mux.HandleFunc("/api/targets", s.handleAPITargets)
	mux.HandleFunc("/api/targets/", s.handleAPITargetRuns)
	mux.HandleFunc("/download/", s.handleDownload)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

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
