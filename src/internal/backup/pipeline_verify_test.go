package backup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"backup-operator/crypto"
	"backup-operator/internal/labels"
	"backup-operator/internal/meta"
	"backup-operator/internal/secrets"
	"backup-operator/verifier"

	"github.com/go-logr/logr"
)

func TestVerifierBudget(t *testing.T) {
	// No deadline -> fallback cap.
	if got := verifierBudget(context.Background()); got != 30*time.Minute {
		t.Errorf("no-deadline budget = %v, want 30m", got)
	}

	// Ample remaining time -> capped at 30m (not 70% of a huge remaining).
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if got := verifierBudget(ctx); got != 30*time.Minute {
		t.Errorf("1h-remaining budget = %v, want capped 30m", got)
	}

	// Modest remaining time -> 70% of it, leaving headroom for uploadMeta.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel2()
	got := verifierBudget(ctx2)
	if got > 7*time.Minute+time.Second || got < 6*time.Minute {
		t.Errorf("10m-remaining budget = %v, want ~7m (70%%)", got)
	}
	if got >= 10*time.Minute {
		t.Errorf("budget %v must leave headroom below the run deadline", got)
	}
}

// slowVerifier blocks until its context ends and reports the context error,
// standing in for a Phase-2 restore that does not fit in the budget.
type slowVerifier struct{ mode string }

func (v *slowVerifier) Mode() string { return v.mode }
func (v *slowVerifier) Verify(ctx context.Context, _ verifier.Input) (*meta.RestoreVerificationResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// failingVerifier errors immediately — a genuine hard failure.
type failingVerifier struct{}

func (failingVerifier) Mode() string { return labels.RestoreVerificationFull }
func (failingVerifier) Verify(context.Context, verifier.Input) (*meta.RestoreVerificationResult, error) {
	return nil, errors.New("psql: connection refused")
}

func budgetTestPipeline(v verifier.Verifier) (*Pipeline, *crypto.EphemeralIdentity) {
	id, err := crypto.GenerateEphemeralIdentity()
	if err != nil {
		panic(err)
	}
	return &Pipeline{
		verifierFactory: func(string, string, logr.Logger) (verifier.Verifier, error) { return v, nil },
	}, id
}

// TestRunRestoreVerifier_BudgetTimeout_CarriesVerdictWithFreshClock regresses
// the "DB pod spawned every run" bug: when the verifier blows its budget, the
// previous verdict must come back with CompletedAt advanced to now so
// ShouldVerify sees the interval as NOT elapsed on the next run.
func TestRunRestoreVerifier_BudgetTimeout_CarriesVerdictWithFreshClock(t *testing.T) {
	p, id := budgetTestPipeline(&slowVerifier{mode: labels.RestoreVerificationFull})
	src := &secrets.Source{TargetName: "t", DBType: "postgres", RestoreVerificationMode: labels.RestoreVerificationFull, RestoreVerificationInterval: time.Hour}
	// Run context with a 400 ms deadline → verifier budget = 280 ms; the run
	// itself is still alive when the sub-context expires.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	old := time.Now().Add(-3 * time.Hour).UTC()
	prev := &meta.RestoreVerificationResult{Mode: labels.RestoreVerificationFull, Verdict: meta.VerificationMatch, Summary: "all 12 tables restored", CompletedAt: old}

	before := time.Now()
	res := p.runRestoreVerifier(ctx, src, "/nonexistent", id, nil, nil, prev, logr.Discard())
	if res == nil {
		t.Fatal("budget timeout must yield a carried-forward result, not nil")
	}
	if res.Verdict != meta.VerificationMatch {
		t.Errorf("verdict = %q, want the previous %q carried forward", res.Verdict, meta.VerificationMatch)
	}
	if !res.CompletedAt.After(before.Add(-time.Second)) || res.CompletedAt.Equal(old) {
		t.Errorf("CompletedAt must be re-stamped to now, got %v (prev %v)", res.CompletedAt, old)
	}
	if res.Error != "" {
		t.Errorf("a carried-forward match must not carry an Error (would flip status/UI to failed): %q", res.Error)
	}
	if !strings.Contains(res.Summary, "exceeded") || !strings.Contains(res.Summary, "all 12 tables restored") {
		t.Errorf("summary must note the budget overrun and keep the prior summary, got %q", res.Summary)
	}
	if ctx.Err() != nil {
		t.Fatal("test setup: run context expired before the assertion — widen the deadline")
	}
	// The next run must now treat the interval as not elapsed.
	latest := &meta.MetaFile{RestoreVerification: res}
	if should, reason := verifier.ShouldVerify(src, latest, time.Now()); should {
		t.Errorf("ShouldVerify must be false right after a budget timeout (reason=%q)", reason)
	}
}

// TestRunRestoreVerifier_BudgetTimeout_FirstVerification: nothing to carry →
// an explanatory Skipped result with a fresh clock, not nil.
func TestRunRestoreVerifier_BudgetTimeout_FirstVerification(t *testing.T) {
	p, id := budgetTestPipeline(&slowVerifier{mode: labels.RestoreVerificationSchemaOnly})
	src := &secrets.Source{TargetName: "t", DBType: "postgres", RestoreVerificationMode: labels.RestoreVerificationSchemaOnly}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	res := p.runRestoreVerifier(ctx, src, "/nonexistent", id, nil, nil, nil, logr.Discard())
	if res == nil || res.Verdict != meta.VerificationSkipped || res.CompletedAt.IsZero() {
		t.Fatalf("first-verification budget timeout must yield Skipped with a clock, got %+v", res)
	}
	if !strings.Contains(res.Error, "time budget") {
		t.Errorf("Error must explain the budget overrun, got %q", res.Error)
	}
}

// TestRunRestoreVerifier_HardFailure_NotMistakenForBudget: a verifier that
// fails for its own reasons while the budget is still open is a real failure
// (Skipped + error), never a carried-forward match.
func TestRunRestoreVerifier_HardFailure_NotMistakenForBudget(t *testing.T) {
	p, id := budgetTestPipeline(failingVerifier{})
	src := &secrets.Source{TargetName: "t", DBType: "postgres", RestoreVerificationMode: labels.RestoreVerificationFull}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	prev := &meta.RestoreVerificationResult{Mode: labels.RestoreVerificationFull, Verdict: meta.VerificationMatch, CompletedAt: time.Now().Add(-3 * time.Hour)}
	res := p.runRestoreVerifier(ctx, src, "/nonexistent", id, nil, nil, prev, logr.Discard())
	if res == nil || res.Verdict != meta.VerificationSkipped || !strings.Contains(res.Error, "connection refused") {
		t.Fatalf("hard failure must surface as Skipped with the error, got %+v", res)
	}
}

func TestBudgetExceededResult_CarriesPrev(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	prev := &meta.RestoreVerificationResult{Mode: "full", Verdict: meta.VerificationMismatch, Error: "3 rows short", CompletedAt: now.Add(-48 * time.Hour), EphemeralRecipientFingerprint: "old"}
	got := budgetExceededResult(prev, "full", now.Add(-time.Minute), now, 7*time.Minute, "new")
	if got.Verdict != meta.VerificationMismatch || got.Error != "3 rows short" || !got.CompletedAt.Equal(now) {
		t.Errorf("carry-forward must keep verdict+error and re-stamp CompletedAt: %+v", got)
	}
	if got.EphemeralRecipientFingerprint != "old" {
		t.Errorf("fingerprint must stay the one that produced the verdict, got %q", got.EphemeralRecipientFingerprint)
	}
	if prev.CompletedAt.Equal(now) {
		t.Error("must not mutate the previous result in place")
	}
}
