package controllers

import "testing"

func TestCronJobNameFor_Short(t *testing.T) {
	got := cronJobNameFor("my-secret")
	want := "backup-my-secret"
	if got != want {
		t.Errorf("cronJobNameFor(%q) = %q, want %q", "my-secret", got, want)
	}
}

func TestCronJobNameFor_ExactLimit(t *testing.T) {
	// "backup-" is 7 chars, so a 45-char secret name gives exactly 52.
	secret := "abcdefghijklmnopqrstuvwxyz0123456789012345678"
	got := cronJobNameFor(secret)
	if len(got) != 52 {
		t.Errorf("expected length 52, got %d: %q", len(got), got)
	}
	want := "backup-" + secret
	if got != want {
		t.Errorf("should not hash when exactly at limit: got %q, want %q", got, want)
	}
}

func TestCronJobNameFor_Long_HasHash(t *testing.T) {
	long := "very-long-secret-name-that-exceeds-the-52-character-kubernetes-limit"
	got := cronJobNameFor(long)
	if len(got) > 52 {
		t.Errorf("name too long: %d > 52: %q", len(got), got)
	}
	// Must end with a hash suffix
	if got[len(got)-9] != '-' {
		t.Errorf("expected hash separator near end, got %q", got)
	}
}

func TestCronJobNameFor_LongCollision(t *testing.T) {
	// Two secrets that share the same 52-char prefix must produce different names.
	a := "very-long-prefix-shared-between-two-secrets-aaaaaaaaa"
	b := "very-long-prefix-shared-between-two-secrets-bbbbbbbbb"
	gotA := cronJobNameFor(a)
	gotB := cronJobNameFor(b)
	if gotA == gotB {
		t.Errorf("collision: both produced %q", gotA)
	}
}
