package controllers

import (
	"context"
	"testing"

	"backup-operator/api/v1alpha1"

	"github.com/go-logr/logr"
)

// TestRefresh_ParseErrorKeepsTargetSticky regresses the "monitoring goes dark"
// bug: when a still-present BackupSource fails to flatten (e.g. an unsafe
// targetName typed during an edit), the refresher must keep its target's
// series rather than sweeping them — otherwise BackupOverdue can never fire
// on the now-absent last_success series and existing alerts silently resolve.
func TestRefresh_ParseErrorKeepsTargetSticky(t *testing.T) {
	src := testSource("prod-db", "backup")
	src.Spec.TargetName = "../evil"
	c := newFakeClient(t, src)
	r := &MetricsRefresher{
		Client:         c,
		Namespace:      "backup",
		Logger:         logr.Discard(),
		trackedTargets: map[string]bool{"prod": true},
		secretToTarget: map[string]string{"backup/prod-db": "prod"},
	}

	r.refresh(context.Background())

	if !r.trackedTargets["prod"] {
		t.Error("a source that fails to flatten must keep its target's metrics sticky, not delete them")
	}
}

// TestRefresh_RemovedSourceDropsTarget confirms the complementary case: a
// source that is genuinely gone still has its target swept and its
// secretToTarget entry pruned.
func TestRefresh_RemovedSourceDropsTarget(t *testing.T) {
	c := newFakeClient(t)
	r := &MetricsRefresher{
		Client:         c,
		Namespace:      "backup",
		Logger:         logr.Discard(),
		trackedTargets: map[string]bool{"gone": true},
		secretToTarget: map[string]string{"backup/gone-db": "gone"},
	}

	r.refresh(context.Background())

	if r.trackedTargets["gone"] {
		t.Error("a removed source's target must be swept from tracking")
	}
	if _, ok := r.secretToTarget["backup/gone-db"]; ok {
		t.Error("secretToTarget must be pruned for a removed source")
	}
}

// TestRefresh_NoDataStillWritesNextSchedule: before the first run lands, the
// refresher still keeps status.nextScheduleTime current so `kubectl get`
// answers "when will this run" from day one.
func TestRefresh_NoDataStillWritesNextSchedule(t *testing.T) {
	src := testSource("fresh", "backup")
	c := newFakeClient(t, src)
	r := &MetricsRefresher{Client: c, Namespace: "backup", Logger: logr.Discard(), DefaultSchedule: "0 2 * * *"}

	r.refresh(context.Background())

	got := getSource(t, c, "fresh", "backup")
	if got.Status.NextScheduleTime == nil {
		t.Error("nextScheduleTime must be set even with no run history")
	}
	if got.Status.LastRunStatus != "" {
		t.Errorf("no run yet, lastRunStatus must stay empty, got %q", got.Status.LastRunStatus)
	}
	_ = v1alpha1.RunStatusSuccess
}
