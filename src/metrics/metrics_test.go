package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestRegister(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	Register(reg)

	// Verify at least the core metrics are registered by gathering.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	// After registration but before any observation, only those metrics
	// with initial values appear. We just check that no panic occurred
	// and registration succeeded.
	_ = mfs
}

func TestSettersDoNotPanic(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	Register(reg)

	// Exercise every setter to verify label cardinality correctness.
	ObserveDumpDuration("t", "postgres", 5*time.Second)
	ObserveUploadDuration("t", "dest", "sftp", 3*time.Second)
	SetDumpSize("t", 42000)
	SetDumpSizeChangeRatio("t", 0.95)
	SetTableCount("t", 12)
	SetTableRowCount("t", "users", 500)
	SetSchemaChanged("t", true)
	SetLastRunAnomalies("t", 2)
	SetLastRunStatus("t", true)
	SetLastRunDuration("t", "postgres", 42*time.Second)
	SetLastSuccess("t", "dest", time.Now())
	SetDestinationFailed("t", "dest", false)
	SetAnalyzerBaselineUnavailable("t", false)
	SetRetentionLastStatus("t", "dest", true)
	SetRetentionLastDeleted("t", "dest", 3)
}

func TestAnalyzerBaselineUnavailable_GaugeRoundtrip(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	Register(reg)

	SetAnalyzerBaselineUnavailable("t1", true)
	SetAnalyzerBaselineUnavailable("t2", false)

	got := map[string]float64{}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "backup_operator_analyzer_baseline_unavailable" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "target" {
					got[l.GetValue()] = m.GetGauge().GetValue()
				}
			}
		}
	}
	if got["t1"] != 1.0 {
		t.Errorf("t1 should be 1, got %v", got["t1"])
	}
	if got["t2"] != 0.0 {
		t.Errorf("t2 should be 0, got %v", got["t2"])
	}
	// DeleteTargetMetrics must remove the gauge so a deleted source does
	// not leave a stale 0 hanging around.
	DeleteTargetMetrics("t1")
	mfs, _ = reg.Gather()
	for _, mf := range mfs {
		if mf.GetName() == "backup_operator_analyzer_baseline_unavailable" {
			for _, m := range mf.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "target" && l.GetValue() == "t1" {
						t.Error("t1 should have been deleted")
					}
				}
			}
		}
	}
}

func TestDeleteTargetMetrics(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	Register(reg)

	SetDumpSize("target-del", 100)
	SetLastRunStatus("target-del", true)

	// Should not panic even if some label combinations don't exist.
	DeleteTargetMetrics("target-del")
}
