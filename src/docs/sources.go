package docs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// source is one markdown file the portal renders. Path is relative to
// DocsDir and always slash-separated, because it is also the key that
// cross-file links are resolved against.
type source struct {
	Slug  string
	Path  string
	Title string // fallback when the file has no H1
	Kind  string // "markdown" or "adr"
}

// topicOrder is the sidebar order for docs/*.md. Files not listed here
// follow alphabetically, so adding a topic never requires touching Go.
var topicOrder = []string{
	"getting-started.md", "architecture.md", "crds.md", "configuration.md",
	"operations.md", "failure-modes.md", "observability.md", "encryption.md",
	"storage-layout.md", "compliance.md", "helm.md", "development.md",
	"extending.md", "conventions.md",
}

// discoverSources lists README.md and CLAUDE.md (both required) plus
// docs/*.md and docs/adr/*.md (optional — an image built from an older
// tree only has the two root files).
func discoverSources(dir string) ([]source, error) {
	out := []source{
		{Slug: "readme", Path: "README.md", Title: "User Guide", Kind: "markdown"},
		{Slug: "claude-md", Path: "CLAUDE.md", Title: "Contributor Guide", Kind: "markdown"},
	}

	topics, err := listMarkdown(filepath.Join(dir, "docs"))
	if err != nil {
		return nil, err
	}
	rank := make(map[string]int, len(topicOrder))
	for i, name := range topicOrder {
		rank[name] = i
	}
	sort.SliceStable(topics, func(i, j int) bool {
		ri, iok := rank[topics[i]]
		rj, jok := rank[topics[j]]
		switch {
		case iok && jok:
			return ri < rj
		case iok != jok:
			return iok
		default:
			return topics[i] < topics[j]
		}
	})
	for _, name := range topics {
		out = append(out, source{
			Slug: strings.TrimSuffix(name, ".md"), Path: "docs/" + name,
			Title: strings.TrimSuffix(name, ".md"), Kind: "markdown",
		})
	}

	adrs, err := listMarkdown(filepath.Join(dir, "docs", "adr"))
	if err != nil {
		return nil, err
	}
	sort.Strings(adrs)
	for _, name := range adrs {
		if name == "README.md" {
			// The index goes into the sidebar, ahead of the individual records.
			out = append(out, source{Slug: "adr", Path: "docs/adr/README.md", Title: "Decisions", Kind: "markdown"})
		}
	}
	for _, name := range adrs {
		if name == "README.md" {
			continue
		}
		out = append(out, source{
			Slug: "adr-" + strings.TrimSuffix(name, ".md"), Path: "docs/adr/" + name,
			Title: strings.TrimSuffix(name, ".md"), Kind: "adr",
		})
	}
	return out, nil
}

// listMarkdown returns the *.md file names directly inside dir. A missing
// dir is not an error.
func listMarkdown(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("docs: list %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// importRE matches a Claude Code memory import on its own line
// (`@docs/conventions.md`). Rendered literally it would be a stray token, so
// the portal turns it into a link to the imported page.
var importRE = regexp.MustCompile(`(?m)^@(\S+\.md)[ \t]*$`)

func expandImports(src []byte) []byte {
	return importRE.ReplaceAll(src, []byte("See [$1]($1)."))
}

// hrefRE matches the href of every rendered link. goldmark always emits
// double-quoted attributes.
var hrefRE = regexp.MustCompile(`href="([^"]*)"`)

// rewriteLinks turns repository-relative markdown links into the SPA's hash
// routes so the same files read correctly on GitHub and in the portal:
//
//	crds.md#61-backupsourcespec  →  #crds/61-backupsourcespec
//	#quick-start                 →  #readme/quick-start (in-page anchor)
//	docs/                        →  the first page under that directory
//
// External links, links to non-markdown files and targets the portal does
// not serve are left untouched.
func rewriteLinks(html, from, slug string, slugs map[string]string) string {
	base := path.Dir(from)
	return hrefRE.ReplaceAllStringFunc(html, func(m string) string {
		href := hrefRE.FindStringSubmatch(m)[1]
		if href == "" || strings.Contains(href, "://") || strings.HasPrefix(href, "mailto:") {
			return m
		}
		if strings.HasPrefix(href, "#") {
			if strings.Contains(href, "/") { // already an SPA route
				return m
			}
			return `href="#` + slug + "/" + href[1:] + `"`
		}
		target, anchor, _ := strings.Cut(href, "#")
		resolved := path.Clean(path.Join(base, target))
		to, ok := slugs[resolved]
		if !ok && (strings.HasSuffix(target, "/") || path.Ext(target) == "") {
			to, ok = firstUnder(resolved, slugs)
		}
		if !ok {
			return m
		}
		if anchor != "" {
			return `href="#` + to + "/" + anchor + `"`
		}
		return `href="#` + to + `"`
	})
}

// firstUnder resolves a directory link: its README.md if served, else the
// first page in sidebar order that lives in that directory.
func firstUnder(dir string, slugs map[string]string) (string, bool) {
	if s, ok := slugs[dir+"/README.md"]; ok {
		return s, true
	}
	best := ""
	for p := range slugs {
		if path.Dir(p) == dir && (best == "" || p < best) {
			best = p
		}
	}
	if best == "" {
		return "", false
	}
	return slugs[best], true
}
