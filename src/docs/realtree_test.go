package docs

import (
	"regexp"
	"testing"

	"github.com/go-logr/logr"
)

// TestRealTree loads the repository's own documentation and fails on any
// markdown link that resolves to a docs page the portal does not serve, so
// a renamed file or a typo in a cross-reference is caught by `go test`.
func TestRealTree(t *testing.T) {
	s, err := New(Config{Addr: ":0", DocsDir: "../..", Logger: logr.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	dangling := regexp.MustCompile(`href="([^"#:]*\.md)(#[^"]*)?"`)
	for slug, p := range s.pages {
		for _, m := range dangling.FindAllStringSubmatch(p.HTML, -1) {
			t.Errorf("%s (%s): unresolved link %s", slug, p.Source, m[1])
		}
	}
	ids := regexp.MustCompile(`id="([^"]+)"`)
	route := regexp.MustCompile(`href="#([^"/]+)/([^"]+)"`)
	for slug, p := range s.pages {
		for _, m := range route.FindAllStringSubmatch(p.HTML, -1) {
			target, ok := s.pages[m[1]]
			if !ok {
				t.Errorf("%s: link to unknown page %q", slug, m[1])
				continue
			}
			found := false
			for _, id := range ids.FindAllStringSubmatch(target.HTML, -1) {
				if id[1] == m[2] {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s (%s): anchor #%s not found on page %s", slug, p.Source, m[2], m[1])
			}
		}
	}
	if len(s.pages) < 100 {
		t.Fatalf("expected the docs/ tree to load, got %d pages", len(s.pages))
	}
}
