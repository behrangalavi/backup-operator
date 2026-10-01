package scheduler

import (
	"strings"
	"testing"
)

// FuzzApplyJitter pins the invariants a schedule rewrite must never break:
// the result is deterministic for the same inputs, has the same field count
// as the input, only the minute field may change, and a changed minute is
// still a valid 0-59 literal.
func FuzzApplyJitter(f *testing.F) {
	for _, s := range []string{"0 2 * * *", "15 2 * * *", "*/15 * * * *", "0,30 * * * *", "0-30 * * * *", "* * * * *", "@daily", "", "0 2 * *", "0 2 * * * *"} {
		f.Add(s, 60, "prod-db")
		f.Add(s, 0, "prod-db")
		f.Add(s, 7, "x")
	}
	f.Fuzz(func(t *testing.T, schedule string, jitter int, name string) {
		if jitter < -1 || jitter > 60 {
			return
		}
		got := ApplyJitter(schedule, jitter, name)
		if again := ApplyJitter(schedule, jitter, name); again != got {
			t.Fatalf("non-deterministic: %q vs %q", got, again)
		}
		in, out := strings.Fields(schedule), strings.Fields(got)
		if len(in) != 5 {
			if got != schedule {
				t.Fatalf("non-5-field schedule %q must be returned untouched, got %q", schedule, got)
			}
			return
		}
		if len(out) != 5 {
			t.Fatalf("field count changed: %q → %q", schedule, got)
		}
		for i := 1; i < 5; i++ {
			if in[i] != out[i] {
				t.Fatalf("field %d changed: %q → %q", i, schedule, got)
			}
		}
		if out[0] != in[0] {
			var m int
			if _, err := parseMinute(out[0], &m); err != nil || m < 0 || m > 59 {
				t.Fatalf("rewritten minute %q is not a literal 0-59 (from %q)", out[0], schedule)
			}
		}
	})
}

func parseMinute(s string, out *int) (bool, error) {
	n := 0
	if s == "" {
		return false, errNotLiteral
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false, errNotLiteral
		}
		n = n*10 + int(c-'0')
	}
	*out = n
	return true, nil
}

type notLiteral struct{}

func (notLiteral) Error() string { return "not a literal minute" }

var errNotLiteral = notLiteral{}
