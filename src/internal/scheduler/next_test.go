package scheduler

import (
	"testing"
	"time"
)

func TestNextRun(t *testing.T) {
	now := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	next, ok := NextRun("0 2 * * *", now)
	if !ok || !next.Equal(time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC)) {
		t.Errorf("NextRun = %v, %v", next, ok)
	}
	if _, ok := NextRun("@daily", now); !ok {
		t.Error("descriptor schedules are valid for Kubernetes CronJobs and must parse")
	}
	if _, ok := NextRun("99 2 * * *", now); ok {
		t.Error("minute 99 must be rejected")
	}
	if _, ok := NextRun("", now); ok {
		t.Error("empty schedule has no next run")
	}
}

func TestValidateSchedule(t *testing.T) {
	if err := ValidateSchedule("*/15 * * * *"); err != nil {
		t.Errorf("valid schedule rejected: %v", err)
	}
	if err := ValidateSchedule("0 2 * *"); err == nil {
		t.Error("4-field schedule must be rejected")
	}
}
