package verifier

import (
	"testing"
	"time"

	"backup-operator/internal/labels"
	"backup-operator/internal/meta"
	"backup-operator/internal/secrets"
)

func TestShouldVerify_NilSource(t *testing.T) {
	got, _ := ShouldVerify(nil, &meta.MetaFile{}, time.Now())
	if got {
		t.Error("nil source must not verify")
	}
}

func TestShouldVerify_ModeOff(t *testing.T) {
	cases := []string{"", labels.RestoreVerificationOff}
	for _, mode := range cases {
		src := &secrets.Source{RestoreVerificationMode: mode}
		got, reason := ShouldVerify(src, &meta.MetaFile{}, time.Now())
		if got {
			t.Errorf("mode=%q: should not verify (reason: %s)", mode, reason)
		}
	}
}

func TestShouldVerify_NoPriorRun(t *testing.T) {
	src := &secrets.Source{RestoreVerificationMode: labels.RestoreVerificationStreamValidate}
	got, reason := ShouldVerify(src, nil, time.Now())
	if got {
		t.Errorf("no prior run: should not verify (reason: %s)", reason)
	}
}

// First-time verification: prior run exists but never verified → run it.
func TestShouldVerify_FirstVerification(t *testing.T) {
	src := &secrets.Source{
		RestoreVerificationMode:     labels.RestoreVerificationStreamValidate,
		RestoreVerificationInterval: 168 * time.Hour,
	}
	prev := &meta.MetaFile{Status: meta.StatusSuccess}
	got, reason := ShouldVerify(src, prev, time.Now())
	if !got {
		t.Errorf("first verification should run (reason: %s)", reason)
	}
}

// CompletedAt zero-value also counts as "never verified".
func TestShouldVerify_ZeroCompletedAt(t *testing.T) {
	src := &secrets.Source{RestoreVerificationMode: labels.RestoreVerificationStreamValidate}
	prev := &meta.MetaFile{
		RestoreVerification: &meta.RestoreVerificationResult{Mode: labels.RestoreVerificationStreamValidate},
	}
	got, _ := ShouldVerify(src, prev, time.Now())
	if !got {
		t.Error("zero CompletedAt should trigger verification")
	}
}

func TestShouldVerify_IntervalNotElapsed(t *testing.T) {
	now := time.Now()
	src := &secrets.Source{
		RestoreVerificationMode:     labels.RestoreVerificationStreamValidate,
		RestoreVerificationInterval: 168 * time.Hour,
	}
	prev := &meta.MetaFile{
		RestoreVerification: &meta.RestoreVerificationResult{
			CompletedAt: now.Add(-1 * time.Hour), // verified an hour ago
		},
	}
	got, reason := ShouldVerify(src, prev, now)
	if got {
		t.Errorf("interval not elapsed: should not verify (reason: %s)", reason)
	}
}

func TestShouldVerify_IntervalElapsed(t *testing.T) {
	now := time.Now()
	src := &secrets.Source{
		RestoreVerificationMode:     labels.RestoreVerificationStreamValidate,
		RestoreVerificationInterval: 24 * time.Hour,
	}
	prev := &meta.MetaFile{
		RestoreVerification: &meta.RestoreVerificationResult{
			CompletedAt: now.Add(-25 * time.Hour),
		},
	}
	got, reason := ShouldVerify(src, prev, now)
	if !got {
		t.Errorf("interval elapsed: should verify (reason: %s)", reason)
	}
}

// Interval == 0 is treated as "use default", not "never verify". This
// matches the parser convention (0 = annotation absent, fall back to
// default at apply time).
func TestShouldVerify_IntervalZeroUsesDefault(t *testing.T) {
	now := time.Now()
	src := &secrets.Source{
		RestoreVerificationMode:     labels.RestoreVerificationStreamValidate,
		RestoreVerificationInterval: 0,
	}
	// Default is 168h — set CompletedAt to 200h ago so we're past it.
	prev := &meta.MetaFile{
		RestoreVerification: &meta.RestoreVerificationResult{
			CompletedAt: now.Add(-200 * time.Hour),
		},
	}
	got, _ := ShouldVerify(src, prev, now)
	if !got {
		t.Error("interval=0 should fall back to default and trigger when default elapsed")
	}
}

func TestFailureResult_PopulatesFields(t *testing.T) {
	started := time.Now().Add(-3 * time.Second)
	r := FailureResult(labels.RestoreVerificationStreamValidate, started, errExample{"boom"}, "abc123")
	if r.Mode != labels.RestoreVerificationStreamValidate {
		t.Errorf("Mode = %q", r.Mode)
	}
	if r.Verdict != meta.VerificationSkipped {
		t.Errorf("Verdict = %q, want %q", r.Verdict, meta.VerificationSkipped)
	}
	if r.Error != "boom" {
		t.Errorf("Error = %q", r.Error)
	}
	if r.EphemeralRecipientFingerprint != "abc123" {
		t.Errorf("Fingerprint = %q", r.EphemeralRecipientFingerprint)
	}
	if r.DurationSeconds < 1 {
		t.Errorf("DurationSeconds = %v, expected >= 1", r.DurationSeconds)
	}
}

type errExample struct{ msg string }

func (e errExample) Error() string { return e.msg }
