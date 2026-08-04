package main

import "testing"

func TestParsePullSecrets(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"regcred", []string{"regcred"}},
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , , b ", []string{"a", "b"}}, // trims + skips blanks
	}
	for _, c := range cases {
		got := parsePullSecrets(c.in)
		if len(got) != len(c.want) {
			t.Errorf("parsePullSecrets(%q) len = %d, want %d (%v)", c.in, len(got), len(c.want), got)
			continue
		}
		for i := range c.want {
			if got[i].Name != c.want[i] {
				t.Errorf("parsePullSecrets(%q)[%d] = %q, want %q", c.in, i, got[i].Name, c.want[i])
			}
		}
	}
}
