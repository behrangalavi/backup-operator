package backup

import (
	"context"
	"testing"
	"time"
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
