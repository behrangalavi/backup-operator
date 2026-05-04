package docs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
)

// heading is a flattened representation used by the SPA's in-page TOC.
type heading struct {
	Level int    `json:"level"`
	Text  string `json:"text"`
	ID    string `json:"id"`
}

// renderMarkdown converts a markdown source into HTML and returns the H1
// title (if any) plus the heading outline. We use GitHub-flavoured Markdown
// (tables, fences, autolinks, strikethrough) and unsafe HTML rendering —
// the markdown source is trusted (CLAUDE.md / README.md from the repo).
func renderMarkdown(src []byte) (htmlOut string, headings []heading, h1 string) {
	md := goldmark.New(
		goldmark.WithExtensions(
			extension.GFM,
			extension.Footnote,
		),
		goldmark.WithParserOptions(
			parser.WithAutoHeadingID(),
		),
		goldmark.WithRendererOptions(
			html.WithUnsafe(),
			html.WithXHTML(),
		),
	)
	root := md.Parser().Parse(text.NewReader(src))

	// Walk for headings — goldmark assigned IDs already.
	ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		hd, ok := n.(*ast.Heading)
		if !ok {
			return ast.WalkContinue, nil
		}
		txt := nodeText(src, hd)
		id := ""
		if hd.Attributes() != nil {
			if v, found := hd.AttributeString("id"); found {
				if b, ok := v.([]byte); ok {
					id = string(b)
				} else if s, ok := v.(string); ok {
					id = s
				}
			}
		}
		if id == "" {
			id = slugify(txt)
		}
		if hd.Level == 1 && h1 == "" {
			h1 = txt
		}
		// Skip the H1 from the TOC (it's the page title).
		if hd.Level >= 2 && hd.Level <= 3 {
			headings = append(headings, heading{Level: hd.Level, Text: txt, ID: id})
		}
		return ast.WalkContinue, nil
	})

	var buf bytes.Buffer
	if err := md.Renderer().Render(&buf, src, root); err != nil {
		return fmt.Sprintf("<p>render error: %s</p>", err), nil, ""
	}
	return buf.String(), headings, h1
}

func nodeText(src []byte, n ast.Node) string {
	var b strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if t, ok := c.(*ast.Text); ok {
			b.Write(t.Segment.Value(src))
		} else {
			b.WriteString(nodeText(src, c))
		}
	}
	return b.String()
}

var slugCleanRE = regexp.MustCompile(`-+`)

func slugify(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
		case unicode.IsSpace(r), r == '-', r == '_':
			b.WriteRune('-')
		}
	}
	return strings.Trim(slugCleanRE.ReplaceAllString(b.String(), "-"), "-")
}

// writeJSON serialises v with a 2xx status. JSON errors are extremely unlikely
// for our value shapes; on failure we emit a minimal text body so the caller
// sees something rather than a silent zero-length response.
func (s *Server) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		s.cfg.Logger.Error(err, "encode docs JSON")
		http.Error(w, "encode failed", http.StatusInternalServerError)
	}
}
