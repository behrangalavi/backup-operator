package docs

import "strings"

// shellHTML returns the SPA envelope. The page list, page bodies, and
// tech-stack data are loaded over the JSON API by docs.js — keeping the
// HTML small means the index is fast even on slow connections.
func (s *Server) shellHTML() string {
	version := s.cfg.Version
	if version == "" {
		version = "dev"
	}
	html := `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1"/>
  <title>Backup Operator — Documentation</title>
  <link rel="stylesheet" href="/static/style.css"/>
</head>
<body>
  <aside id="sidebar">
    <div class="brand">
      <span class="dot"></span>
      <span>Backup Operator</span>
      <span class="version">` + escapeHTML(version) + `</span>
    </div>
    <input id="search" type="search" placeholder="Search…" aria-label="Search documentation"/>
    <nav id="nav"></nav>
    <div class="sidebar-foot">
      <a href="/api/pages" target="_blank">JSON</a> ·
      <a href="https://github.com/mogenius/backup-operator" target="_blank">GitHub</a>
    </div>
  </aside>
  <main id="main">
    <article id="page" class="page-loading">Loading…</article>
    <aside id="toc" aria-label="On this page"></aside>
  </main>
  <script src="/static/docs.js"></script>
</body>
</html>`
	return html
}

func escapeHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}
