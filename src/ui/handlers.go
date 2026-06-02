package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"backup-operator/internal/labels"
	"backup-operator/internal/secrets"
)

// handleIndex renders the namespace overview at /legacy.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/legacy" {
		http.NotFound(w, r)
		return
	}
	targets, err := s.data.listTargets(r.Context())
	if err != nil {
		s.cfg.Logger.Error(err, "list targets")
		renderError(w, http.StatusInternalServerError, "failed to load targets")
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

// handleTarget renders the per-target run history at /legacy/target/<name>.
func (s *Server) handleTarget(w http.ResponseWriter, r *http.Request) {
	name := trimPrefixPath(r.URL.Path, "/legacy/target/")
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
		s.cfg.Logger.Error(err, "list targets (API)")
		renderError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if hasPaginationParams(r) {
		limit, offset := parsePagination(r, 50)
		total := len(targets)
		if offset > total {
			offset = total
		}
		end := offset + limit
		if end > total {
			end = total
		}
		writeJSON(w, http.StatusOK, paginatedResponse{
			Items:  targets[offset:end],
			Total:  total,
			Limit:  limit,
			Offset: offset,
		})
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
		renderError(w, http.StatusNotFound, "target not found")
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
		suffix := labels.DumpSuffix(run.Compression)
		objectPath = strings.TrimSuffix(run.Path, ".meta.json") + "." + suffix
		contentType = "application/octet-stream"
		filename = fmt.Sprintf("%s-%s.%s", target, timestamp, suffix)
	default:
		http.NotFound(w, r)
		return
	}

	if len(detail.Destinations) == 0 {
		renderError(w, http.StatusServiceUnavailable, "no destinations configured for this target")
		return
	}

	// If ?destination= is specified, download from that specific destination.
	// Otherwise, try destinations in order; first one that yields the object wins.
	wantDest := r.URL.Query().Get("destination")
	destsToTry := detail.Destinations
	if wantDest != "" {
		var found *secrets.Destination
		for _, d := range detail.Destinations {
			if d.Name == wantDest {
				found = d
				break
			}
		}
		if found == nil {
			renderError(w, http.StatusBadRequest, "destination not found: "+wantDest)
			return
		}
		destsToTry = []*secrets.Destination{found}
	}

	if err := s.streamFromFirstDest(w, r, destsToTry, objectPath, contentType, filename, target, kind); err != nil {
		s.cfg.Logger.Error(err, "download failed", "target", target, "kind", kind)
		renderError(w, http.StatusBadGateway, "failed to retrieve backup from storage")
	}
}

// streamFromFirstDest tries each destination in order; the first one that
// yields the object wins. Extracted from handleDownload so the io.ReadCloser
// lifetime is scoped to the function call rather than deferred in a loop.
func (s *Server) streamFromFirstDest(w http.ResponseWriter, r *http.Request, dests []*secrets.Destination, objectPath, contentType, filename, target, kind string) error {
	for _, dest := range dests {
		st, err := s.storageFor(dest, "download")
		if err != nil {
			continue
		}
		rc, err := st.Get(r.Context(), objectPath)
		if err != nil {
			continue
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
		w.Header().Set("X-Source-Destination", dest.Name)
		_, copyErr := io.Copy(w, rc)
		_ = rc.Close()
		if copyErr != nil {
			s.cfg.Logger.Error(copyErr, "stream download", "target", target, "kind", kind)
		}
		return nil
	}
	return fmt.Errorf("no destination served the artifact")
}

// paginatedResponse wraps any list endpoint with offset-based pagination
// metadata. Used when the client explicitly passes ?limit= or ?offset=.
// Without those params, endpoints return a bare array for backward compat.
type paginatedResponse struct {
	Items  any `json:"items"`
	Total  int `json:"total"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

// hasPaginationParams returns true when the request carries explicit
// limit or offset query parameters. Used to decide between paginated
// envelope and backward-compatible bare array responses.
func hasPaginationParams(r *http.Request) bool {
	q := r.URL.Query()
	return q.Get("limit") != "" || q.Get("offset") != ""
}

func parsePagination(r *http.Request, defaultLimit int) (limit, offset int) {
	limit = defaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
