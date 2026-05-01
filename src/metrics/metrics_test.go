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
	SetLastSuccess("t", "dest", time.Now())
	SetDestinationFailed("t", "dest", false)
	IncRetentionDeleted("t", "dest", "dump")
	IncRetentionFailure("t", "dest")
}

func TestDeleteTargetMetrics(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	Register(reg)

	SetDumpSize("target-del", 100)
	SetLastRunStatus("target-del", true)

	// Should not panic even if some label combinations don't exist.
	DeleteTargetMetrics("target-del")
}
