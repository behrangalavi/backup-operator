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

// gaugeValue returns the value of the named gauge for the given label set,
// and whether such a series currently exists in the registry.
func gaugeValue(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) (float64, bool) {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			match := true
			for k, v := range want {
				if labels[k] != v {
					match = false
					break
				}
			}
			if match {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

func TestDeleteDestinationMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	Register(reg)

	// Two destinations for the same target; one will be removed.
	SetRetentionLastStatus("dest-target", "keep", true)
	SetRetentionLastDeleted("dest-target", "keep", 2)
	SetDestinationFailed("dest-target", "keep", false)
	SetRetentionLastStatus("dest-target", "drop", true)
	SetRetentionLastDeleted("dest-target", "drop", 5)
	SetDestinationFailed("dest-target", "drop", true)

	DeleteDestinationMetrics("dest-target", "drop")

	if _, ok := gaugeValue(t, reg, "backup_operator_retention_last_status",
		map[string]string{"target": "dest-target", "destination": "drop"}); ok {
		t.Error("retention_last_status{drop} should be absent after delete")
	}
	if _, ok := gaugeValue(t, reg, "backup_operator_destination_failed",
		map[string]string{"target": "dest-target", "destination": "drop"}); ok {
		t.Error("destination_failed{drop} should be absent after delete")
	}
	// The surviving destination must be untouched.
	if v, ok := gaugeValue(t, reg, "backup_operator_retention_last_deleted_count",
		map[string]string{"target": "dest-target", "destination": "keep"}); !ok || v != 2 {
		t.Errorf("retention_last_deleted_count{keep} = %v present=%v, want 2/true", v, ok)
	}
}

func TestDeleteTableMetric(t *testing.T) {
	reg := prometheus.NewRegistry()
	Register(reg)

	SetTableRowCount("tbl-target", "keep", 100)
	SetTableRowCount("tbl-target", "drop", 200)

	DeleteTableMetric("tbl-target", "drop")

	if _, ok := gaugeValue(t, reg, "backup_operator_table_row_count",
		map[string]string{"target": "tbl-target", "table": "drop"}); ok {
		t.Error("table_row_count{drop} should be absent after delete")
	}
	if v, ok := gaugeValue(t, reg, "backup_operator_table_row_count",
		map[string]string{"target": "tbl-target", "table": "keep"}); !ok || v != 100 {
		t.Errorf("table_row_count{keep} = %v present=%v, want 100/true", v, ok)
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
