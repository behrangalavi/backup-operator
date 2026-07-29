package ui

import "testing"

func TestNormalizeMetricPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Parametric routes collapse to their prefix.
		{"/api/sources/prod-db", "/api/sources/"},
		{"/api/destinations/hetzner", "/api/destinations/"},
		{"/api/targets/prod-users", "/api/targets/"},
		{"/api/trigger/prod-users", "/api/trigger/"},
		{"/api/age-keys/abc123", "/api/age-keys/"},
		{"/download/prod/20260101T020000Z/dump", "/download/"},
		{"/legacy/target/foo", "/legacy/target/"},

		// Fixed routes pass through verbatim.
		{"/", "/"},
		{"/legacy", "/legacy"},
		{"/healthz", "/healthz"},
		{"/api/targets", "/api/targets"},
		{"/api/destinations", "/api/destinations"},
		{"/api/jobs", "/api/jobs"},
		{"/api/settings", "/api/settings"},
		{"/api/settings/export", "/api/settings/export"},
		{"/api/age-keys", "/api/age-keys"},
		{"/api/destination-stats", "/api/destination-stats"},
		{"/api/alerts", "/api/alerts"},
		{"/api/alerts/status", "/api/alerts/status"},

		// Everything else — the SPA catch-all and unknown /api/* 404s —
		// collapses to a single bounded label.
		{"/wp-admin", "other"},
		{"/.env", "other"},
		{"/a/b/c/random", "other"},
		{"/api/does-not-exist", "other"},
		{"/dashboard", "other"},
		{"", "other"},
	}
	for _, c := range cases {
		if got := normalizeMetricPath(c.in); got != c.want {
			t.Errorf("normalizeMetricPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestNormalizeMetricPath_BoundsCardinality asserts that a flood of distinct
// scanner paths all land in the single "other" bucket — the core property
// that stops unbounded histogram-series growth.
func TestNormalizeMetricPath_BoundsCardinality(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range []string{"/x1", "/x2", "/y/z", "/scan/a", "/scan/b", "/..%2f", "/api/nope"} {
		seen[normalizeMetricPath(p)] = true
	}
	if len(seen) != 1 || !seen["other"] {
		t.Errorf("unknown paths must all collapse to a single \"other\" label, got %v", seen)
	}
}
