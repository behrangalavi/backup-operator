package scheduler

import (
	"strconv"
	"strings"
	"testing"
)

// firstField pulls the minute field from a cron expression. Tests use
// it to assert against the offset without re-implementing the join.
func firstField(t *testing.T, schedule string) string {
	t.Helper()
	parts := strings.Fields(schedule)
	if len(parts) != 5 {
		t.Fatalf("expected 5-field schedule, got %d in %q", len(parts), schedule)
	}
	return parts[0]
}

func TestApplyJitter_DefaultBehaviorOnZeroMinute(t *testing.T) {
	// The canonical "0 H * * *" form is the one almost every user
	// types without thinking. Default behaviour MUST jitter it — that
	// is the whole point of fleet protection.
	got := ApplyJitter("0 2 * * *", JitterMinutesUnset, "prod-users-db")
	min := firstField(t, got)
	if min == "0" {
		t.Errorf("default jitter should rewrite minute=0; got %q", got)
	}
	n, err := strconv.Atoi(min)
	if err != nil {
		t.Fatalf("minute field is not numeric: %q", min)
	}
	if n < 0 || n >= 60 {
		t.Errorf("offset %d out of [0,60)", n)
	}
	// Hour and the rest must be preserved verbatim.
	if !strings.HasSuffix(got, " 2 * * *") {
		t.Errorf("non-minute fields mutated: %q", got)
	}
}

func TestApplyJitter_DefaultRespectsExplicitMinute(t *testing.T) {
	// Without an explicit annotation, a non-zero literal minute is
	// treated as deliberate — leave it alone. This is the trust-the-
	// user contract the §18 ADR commits to.
	got := ApplyJitter("15 2 * * *", JitterMinutesUnset, "any-source")
	if got != "15 2 * * *" {
		t.Errorf("default jitter must respect explicit minute; got %q", got)
	}
}

func TestApplyJitter_AnnotationOverridesExplicitMinute(t *testing.T) {
	// When the user sets jitter-minutes explicitly they are opting in
	// to fleet spreading even on a non-zero literal minute — they
	// asked for it.
	got := ApplyJitter("15 2 * * *", 60, "any-source")
	if got == "15 2 * * *" {
		t.Errorf("explicit annotation should override literal minute; got %q", got)
	}
}

func TestApplyJitter_ZeroDisablesEvenOnZeroMinute(t *testing.T) {
	// "I really need exact :00, do not touch" — opt-out path.
	got := ApplyJitter("0 2 * * *", 0, "any-source")
	if got != "0 2 * * *" {
		t.Errorf("jitter-minutes=0 must be a hard opt-out; got %q", got)
	}
}

func TestApplyJitter_DeterministicAcrossCalls(t *testing.T) {
	// Reconciles fire repeatedly. If the offset changed each call we
	// would re-patch the CronJob endlessly and the schedule would
	// drift visibly to operators.
	first := ApplyJitter("0 2 * * *", JitterMinutesUnset, "prod-users-db")
	for i := 0; i < 50; i++ {
		got := ApplyJitter("0 2 * * *", JitterMinutesUnset, "prod-users-db")
		if got != first {
			t.Fatalf("non-deterministic on iteration %d: %q vs %q", i, got, first)
		}
	}
}

func TestApplyJitter_DifferentNamesSpread(t *testing.T) {
	// Sanity check on hash distribution: 20 unique names over a 60-min
	// window should not all land on the same minute. We expect at
	// least 10 distinct minutes — extremely loose to avoid flake, but
	// still catches a constant-output bug.
	seen := map[string]struct{}{}
	for i := 0; i < 20; i++ {
		name := "src-" + strconv.Itoa(i)
		got := ApplyJitter("0 2 * * *", JitterMinutesUnset, name)
		seen[firstField(t, got)] = struct{}{}
	}
	if len(seen) < 10 {
		t.Errorf("hash distribution looks degenerate: %d unique minutes for 20 names", len(seen))
	}
}

func TestApplyJitter_MultiFireSchedulesUnchanged(t *testing.T) {
	// These all express multi-fire intent. Rewriting the minute would
	// silently change "every 15 min" / "twice an hour" / "every minute
	// for a window" into a single fire per hour. Always leave alone.
	cases := []string{
		"*/15 * * * *", // step
		"0,30 2 * * *", // multi-value
		"0-30 2 * * *", // range
		"* * * * *",    // every minute
		"*/5 2 * * *",  // step inside fixed hour
	}
	for _, in := range cases {
		got := ApplyJitter(in, 60, "any-source")
		if got != in {
			t.Errorf("multi-fire schedule rewritten: %q -> %q", in, got)
		}
		// Same with default behaviour.
		got = ApplyJitter(in, JitterMinutesUnset, "any-source")
		if got != in {
			t.Errorf("multi-fire schedule rewritten (default mode): %q -> %q", in, got)
		}
	}
}

func TestApplyJitter_InvalidScheduleUntouched(t *testing.T) {
	// We do not "fix" malformed cron expressions — let K8s reject
	// them with a clear validator error instead of silently producing
	// a different valid one.
	cases := []string{
		"0 2 * *",          // 4 fields
		"",                 // empty
		"not-a-schedule",   // single token
		"0 2 * * * extra",  // 6 fields
	}
	for _, in := range cases {
		if got := ApplyJitter(in, 60, "any-source"); got != in {
			t.Errorf("invalid schedule mutated: %q -> %q", in, got)
		}
	}
}

func TestApplyJitter_WindowCappedAt60(t *testing.T) {
	// jitter-minutes > 60 wraps the hour boundary, which would shift
	// the user's intended hour. Cap is silent to keep the user
	// contract simple ("any number, we'll do the right thing").
	got := ApplyJitter("0 2 * * *", 240, "prod-users-db")
	min, err := strconv.Atoi(firstField(t, got))
	if err != nil {
		t.Fatalf("minute not numeric: %q", got)
	}
	if min < 0 || min >= 60 {
		t.Errorf("oversized window not capped: minute %d", min)
	}
}

func TestApplyJitter_NarrowWindowConstrainsOffset(t *testing.T) {
	// When the user asks for a tighter spread, the offset must stay
	// inside it. Probe many names to make sure no name escapes the
	// window.
	const window = 15
	for i := 0; i < 200; i++ {
		name := "src-" + strconv.Itoa(i)
		got := ApplyJitter("0 2 * * *", window, name)
		min, err := strconv.Atoi(firstField(t, got))
		if err != nil {
			t.Fatalf("minute not numeric: %q", got)
		}
		if min < 0 || min >= window {
			t.Errorf("offset %d for name %q escaped window [0,%d)", min, name, window)
		}
	}
}

func TestApplyJitter_SpacesNormalizedOnRewrite(t *testing.T) {
	// strings.Fields collapses runs of whitespace; the rewritten
	// schedule should be a single-space-joined canonical form.
	got := ApplyJitter("0   2  *  *  *", JitterMinutesUnset, "src")
	if strings.Contains(got, "  ") {
		t.Errorf("rewritten schedule has runs of whitespace: %q", got)
	}
}
