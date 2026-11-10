package docs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr"
)

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverSources_OrderAndKinds(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "README.md", "# R\n")
	writeFile(t, dir, "CLAUDE.md", "# C\n")
	writeFile(t, dir, "docs/zz-new-topic.md", "# Z\n")
	writeFile(t, dir, "docs/crds.md", "# CRDs\n")
	writeFile(t, dir, "docs/getting-started.md", "# GS\n")
	writeFile(t, dir, "docs/adr/0002-b.md", "# B\n")
	writeFile(t, dir, "docs/adr/0001-a.md", "# A\n")
	writeFile(t, dir, "docs/adr/README.md", "# Decisions\n")

	got, err := discoverSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	slugs := make([]string, 0, len(got))
	for _, s := range got {
		slugs = append(slugs, s.Slug+":"+s.Kind)
	}
	want := "readme:markdown claude-md:markdown getting-started:markdown crds:markdown zz-new-topic:markdown adr:markdown adr-0001-a:adr adr-0002-b:adr"
	if strings.Join(slugs, " ") != want {
		t.Fatalf("order/kinds:\n got %s\nwant %s", strings.Join(slugs, " "), want)
	}
}

func TestDiscoverSources_NoDocsDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "README.md", "# R\n")
	writeFile(t, dir, "CLAUDE.md", "# C\n")
	got, err := discoverSources(dir)
	if err != nil || len(got) != 2 {
		t.Fatalf("want the two root files only, got %v (err %v)", got, err)
	}
}

func TestRewriteLinks(t *testing.T) {
	slugs := map[string]string{
		"README.md":             "readme",
		"CLAUDE.md":             "claude-md",
		"docs/crds.md":          "crds",
		"docs/architecture.md":  "architecture",
		"docs/adr/README.md":    "adr",
		"docs/adr/0001-crds.md": "adr-0001-crds",
	}
	cases := []struct {
		from, href, want string
	}{
		{"docs/architecture.md", "crds.md#61-spec", "#crds/61-spec"},
		{"docs/adr/0001-crds.md", "../crds.md", "#crds"},
		{"docs/adr/README.md", "0001-crds.md", "#adr-0001-crds"},
		{"README.md", "docs/", "#architecture"},
		{"README.md", "docs/adr/README.md", "#adr"},
		{"README.md", "#quick-start", "#readme/quick-start"},
		{"README.md", "#readme/quick-start", "#readme/quick-start"},
		{"README.md", "https://example.com/x.md", "https://example.com/x.md"},
		{"README.md", "charts/backup-operator/values.yaml", "charts/backup-operator/values.yaml"},
		{"README.md", "docs/missing.md", "docs/missing.md"},
	}
	for _, c := range cases {
		got := rewriteLinks(`<a href="`+c.href+`">x</a>`, c.from, slugs[c.from], slugs)
		if want := `<a href="` + c.want + `">x</a>`; got != want {
			t.Errorf("%s → %q: got %s, want %s", c.from, c.href, got, want)
		}
	}
}

func TestExpandImports(t *testing.T) {
	got := string(expandImports([]byte("intro\n@docs/conventions.md\nmail @someone here\n")))
	if !strings.Contains(got, "See [docs/conventions.md](docs/conventions.md).") || !strings.Contains(got, "mail @someone here") {
		t.Fatalf("unexpected expansion: %q", got)
	}
}

func TestNew_ServesDocsTree(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "README.md", "# Readme\n\nSee [CRDs](docs/crds.md#spec).\n")
	writeFile(t, dir, "CLAUDE.md", "# Guide\n\n@docs/crds.md\n")
	writeFile(t, dir, "docs/crds.md", "# CRDs\n\n## Spec\n")
	writeFile(t, dir, "docs/adr/README.md", "# Decisions\n")
	writeFile(t, dir, "docs/adr/0001-x.md", "# 0001. X\n")
	s, err := New(Config{Addr: ":0", DocsDir: dir, Logger: logr.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	if p := s.pages["readme"]; p == nil || !strings.Contains(p.HTML, `href="#crds/spec"`) {
		t.Fatalf("README link not rewritten: %+v", p)
	}
	if p := s.pages["claude-md"]; p == nil || !strings.Contains(p.HTML, `href="#crds"`) {
		t.Fatalf("CLAUDE.md import not linked: %+v", p)
	}
	if p := s.pages["adr-0001-x"]; p == nil || p.Kind != "adr" || p.Title != "0001. X" {
		t.Fatalf("ADR page wrong: %+v", p)
	}
	if n := len(s.pageList()); n != 5 {
		t.Fatalf("pageList: want 5 entries, got %d", n)
	}
}
