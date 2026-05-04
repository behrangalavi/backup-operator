// Package docs serves the project documentation on its own HTTP listener.
// CLAUDE.md and README.md are read from disk at startup, rendered through
// goldmark, and exposed under /. Additional generated pages — /tech-stack,
// /api, /metrics-catalog — are produced from the operator's own go.mod and
// the markdown headings.
//
// Run-time reads (rather than go:embed) keep the Go package portable across
// repo layouts: CLAUDE.md and README.md live at the repo root, which a
// package-local embed cannot reach. The Dockerfile copies the files into
// /app/docs; locally a developer points DOCS_DIR at the repo root.
package docs

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// Config controls the docs server. All fields are required; New returns an
// error if anything mandatory is missing rather than silently falling back.
type Config struct {
	Addr    string      // listen address, e.g. ":8083"
	DocsDir string      // filesystem path containing CLAUDE.md, README.md, go.mod
	Logger  logr.Logger // emitted with name "docs"
	Version string      // build version, shown in the footer
}

// staticFS holds the docs SPA assets (style.css, docs.js).
//
//go:embed static
var staticFS embed.FS

// Server bundles the rendered pages so requests are O(1). Pages are rendered
// once at New() time — the source files don't change between rebuilds.
type Server struct {
	cfg   Config
	pages map[string]*page // path → page
	tech  *techStackPage
	mu    sync.RWMutex
}

// page is the rendered representation of a markdown source plus metadata
// the SPA shell needs to render the sidebar.
type page struct {
	Slug    string    // URL slug, e.g. "claude-md"
	Title   string    // first H1 in the document
	HTML    string    // rendered body
	Headings []heading // for the in-page TOC
	Updated time.Time // file mtime
	Source  string    // relative source path (for the footer link)
}

func New(cfg Config) (*Server, error) {
	if cfg.Addr == "" {
		return nil, errors.New("docs: Addr is required")
	}
	if cfg.DocsDir == "" {
		return nil, errors.New("docs: DocsDir is required")
	}
	if _, err := os.Stat(cfg.DocsDir); err != nil {
		return nil, fmt.Errorf("docs: DocsDir %q is not accessible: %w", cfg.DocsDir, err)
	}

	s := &Server{cfg: cfg, pages: make(map[string]*page)}

	// Render the canonical sources. Failure here is fatal — a docs server
	// with no docs is worse than no docs server at all.
	if err := s.loadMarkdown("claude-md", "CLAUDE.md", "Operator Reference"); err != nil {
		return nil, err
	}
	if err := s.loadMarkdown("readme", "README.md", "User Guide"); err != nil {
		return nil, err
	}

	// Try the canonical container layout first (Dockerfile copies
	// src/go.mod to /app/docs/go.mod), then fall back to the repo-root
	// layout used during local `just run` (DOCS_DIR=.. → ../src/go.mod).
	candidates := []string{
		filepath.Join(cfg.DocsDir, "go.mod"),
		filepath.Join(cfg.DocsDir, "src", "go.mod"),
	}
	for _, p := range candidates {
		tech, err := loadTechStack(p)
		if err == nil {
			s.tech = tech
			break
		}
		cfg.Logger.V(1).Info("tech stack candidate not usable", "path", p, "err", err.Error())
	}
	if s.tech == nil {
		cfg.Logger.Info("tech stack page disabled: no readable go.mod under DocsDir", "docsDir", cfg.DocsDir)
	}

	return s, nil
}

func (s *Server) loadMarkdown(slug, filename, title string) error {
	full := filepath.Join(s.cfg.DocsDir, filename)
	raw, err := os.ReadFile(full)
	if err != nil {
		return fmt.Errorf("read %s: %w", full, err)
	}
	html, headings, h1 := renderMarkdown(raw)
	if h1 != "" {
		title = h1
	}
	info, _ := os.Stat(full)
	var updated time.Time
	if info != nil {
		updated = info.ModTime()
	}
	s.pages[slug] = &page{
		Slug: slug, Title: title,
		HTML: html, Headings: headings,
		Updated: updated, Source: filename,
	}
	return nil
}

// Start blocks until ctx is cancelled or the listener fails.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return fmt.Errorf("docs: static subfs: %w", err)
	}
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", s.handleSPA)

	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.cfg.Logger.Info("docs server listening", "addr", s.cfg.Addr,
			"pages", len(s.pages), "techStack", s.tech != nil)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// handleSPA serves the docs shell for any non-asset path. The actual page
// switching is client-side via hash routing; the server returns the same
// HTML envelope and lets docs.js pick the page.
func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.handleAPI(w, r)
		return
	}
	if r.URL.Path != "/" {
		// Allow /docs/<slug> as a clean URL too — collapse to /#<slug>
		// for simplicity.
		http.Redirect(w, r, "/#"+strings.TrimPrefix(r.URL.Path, "/"), http.StatusFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(s.shellHTML()))
}

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/pages":
		s.writeJSON(w, s.pageList())
	case strings.HasPrefix(r.URL.Path, "/api/page/"):
		slug := strings.TrimPrefix(r.URL.Path, "/api/page/")
		s.mu.RLock()
		p, ok := s.pages[slug]
		s.mu.RUnlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		s.writeJSON(w, map[string]any{
			"slug":     p.Slug,
			"title":    p.Title,
			"html":     p.HTML,
			"headings": p.Headings,
			"updated":  p.Updated.UTC().Format(time.RFC3339),
			"source":   p.Source,
		})
	case r.URL.Path == "/api/tech-stack":
		if s.tech == nil {
			http.NotFound(w, r)
			return
		}
		s.writeJSON(w, s.tech)
	default:
		http.NotFound(w, r)
	}
}

// pageList returns the sidebar entries in a stable order: CLAUDE.md first
// (operator reference), README.md second (user guide), then synthetic pages.
func (s *Server) pageList() []map[string]any {
	order := []string{"claude-md", "readme"}
	out := make([]map[string]any, 0, len(order)+2)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, slug := range order {
		p, ok := s.pages[slug]
		if !ok {
			continue
		}
		out = append(out, map[string]any{
			"slug": p.Slug, "title": p.Title, "kind": "markdown",
			"updated": p.Updated.UTC().Format(time.RFC3339),
		})
	}
	if s.tech != nil {
		out = append(out, map[string]any{
			"slug": "tech-stack", "title": "Tech Stack", "kind": "synthetic",
		})
	}
	return out
}
