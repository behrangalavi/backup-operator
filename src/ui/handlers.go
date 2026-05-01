package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	storageFactory "backup-operator/storage/factory"
)

// handleIndex renders the namespace overview at /.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	targets, err := s.data.listTargets(r.Context())
	if err != nil {
		s.cfg.Logger.Error(err, "list targets")
		renderError(w, http.StatusInternalServerError, "failed to load targets: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, "index", struct {
		Namespace string
		Targets   []targetSummary
	}{s.cfg.Namespace, targets}); err != nil {
		s.cfg.Logger.Error(err, "render index")
	}
}

// handleTarget renders the per-target run history at /target/<name>.
func (s *Server) handleTarget(w http.ResponseWriter, r *http.Request) {
	name := trimPrefixPath(r.URL.Path, "/target/")
	if name == "" || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	detail, err := s.data.target(r.Context(), name)
	if err != nil {
		renderError(w, http.StatusNotFound, "target not found: "+name)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, "target", struct {
		Detail *targetDetail
	}{detail}); err != nil {
		s.cfg.Logger.Error(err, "render target detail")
	}
}

func (s *Server) handleAPITargets(w http.ResponseWriter, r *http.Request) {
	targets, err := s.data.listTargets(r.Context())
	if err != nil {
		renderError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, targets)
}

// handleAPITargetRuns serves /api/targets/<name>/runs.
func (s *Server) handleAPITargetRuns(w http.ResponseWriter, r *http.Request) {
	rest := trimPrefixPath(r.URL.Path, "/api/targets/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[1] != "runs" {
		http.NotFound(w, r)
		return
	}
	detail, err := s.data.target(r.Context(), parts[0])
	if err != nil {
		renderError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, detail.Runs)
}

// handleDownload streams either the encrypted dump (.sql.gz.age) or its
// unencrypted meta.json sidecar through the operator from the first
// destination that has the artifact. URL shape:
//
//	/download/<target>/<timestamp>/<kind>      kind ∈ {dump, meta}
//
// The encrypted dump is a pass-through — the operator never decrypts and
// never sees the private key. Decryption happens on the operator's
// machine via `age -d -i ~/age.key` (or the backup-restore CLI).
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	rest := trimPrefixPath(r.URL.Path, "/download/")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 {
		http.NotFound(w, r)
		return
	}
	target, timestamp, kind := parts[0], parts[1], parts[2]

	detail, err := s.data.target(r.Context(), target)
	if err != nil {
		renderError(w, http.StatusNotFound, "target not found")
		return
	}
	run := findRun(detail.Runs, timestamp)
	if run == nil {
		renderError(w, http.StatusNotFound, "run not found")
		return
	}

	// The MetaFile.Path holds the absolute object path of the meta JSON;
	// the encrypted dump lives next to it with a different extension.
	var objectPath, contentType, filename string
	switch kind {
	case "meta":
		objectPath = run.Path
		contentType = "application/json"
		filename = fmt.Sprintf("%s-%s.meta.json", target, timestamp)
	case "dump":
		objectPath = strings.TrimSuffix(run.Path, ".meta.json") + ".sql.gz.age"
		contentType = "application/octet-stream"
		filename = fmt.Sprintf("%s-%s.sql.gz.age", target, timestamp)
	default:
		http.NotFound(w, r)
		return
	}

	if len(detail.Destinations) == 0 {
		renderError(w, http.StatusServiceUnavailable, "no destinations configured for this target")
		return
	}

	// Try destinations in order; first one that yields the object wins.
	// This mirrors the fan-out semantics — every destination should hold
	// the artifact, but tolerating one being temporarily unavailable
	// matches the pipeline's resilience design.
	for _, dest := range detail.Destinations {
		st, err := storageFactory.NewStorage(dest.StorageType, dest.Name, dest.Data, s.cfg.Logger.WithName("download"))
		if err != nil {
			continue
		}
		rc, err := st.Get(r.Context(), objectPath)
		if err != nil {
			continue
		}
		defer rc.Close()

		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
		if _, err := io.Copy(w, rc); err != nil {
			s.cfg.Logger.Error(err, "stream download", "target", target, "kind", kind)
		}
		return
	}
	renderError(w, http.StatusBadGateway, "no destination served the artifact")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
