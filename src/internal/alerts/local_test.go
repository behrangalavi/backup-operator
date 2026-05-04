package alerts

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// fixedRegistry assembles the operator's metric vectors against an isolated
// registry so each test starts from a clean slate.
func fixedRegistry(t *testing.T) (prometheus.Gatherer, *metricSet) {
	t.Helper()
	reg := prometheus.NewRegistry()
	ms := newMetricSet()
	reg.MustRegister(ms.lastSuccess, ms.destFailed, ms.sizeRatio, ms.schemaChanged, ms.lastStatus, ms.anomalies)
	return reg, ms
}

type metricSet struct {
	lastSuccess   *prometheus.GaugeVec
	destFailed    *prometheus.GaugeVec
	sizeRatio     *prometheus.GaugeVec
	schemaChanged *prometheus.GaugeVec
	lastStatus    *prometheus.GaugeVec
	anomalies     *prometheus.GaugeVec
}

func newMetricSet() *metricSet {
	return &metricSet{
		lastSuccess:   prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "backup_operator_last_success_timestamp_seconds"}, []string{"target", "destination"}),
		destFailed:    prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "backup_operator_destination_failed"}, []string{"target", "destination"}),
		sizeRatio:     prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "backup_operator_dump_size_change_ratio"}, []string{"target"}),
		schemaChanged: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "backup_operator_schema_changed"}, []string{"target"}),
		lastStatus:    prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "backup_operator_last_run_status"}, []string{"target"}),
		anomalies:     prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "backup_operator_last_run_anomalies"}, []string{"target"}),
	}
}

func TestLocalProvider_NoSignals_NoAlerts(t *testing.T) {
	reg, _ := fixedRegistry(t)
	p := NewLocalProvider(reg)
	got, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 alerts, got %d: %+v", len(got), got)
	}
}

func TestLocalProvider_Overdue(t *testing.T) {
	reg, ms := fixedRegistry(t)
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-48 * time.Hour).Unix()
	ms.lastSuccess.WithLabelValues("prod-users", "minio").Set(float64(stale))

	p := &LocalProvider{Gatherer: reg, Now: func() time.Time { return now }}
	got, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || got[0].Alertname != "BackupOverdue" {
		t.Fatalf("want BackupOverdue, got %+v", got)
	}
	if got[0].Target != "prod-users" || got[0].Severity != "warning" {
		t.Errorf("unexpected fields: %+v", got[0])
	}
}

func TestLocalProvider_OverdueIgnoresFreshRun(t *testing.T) {
	reg, ms := fixedRegistry(t)
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-1 * time.Hour).Unix()
	ms.lastSuccess.WithLabelValues("prod-users", "minio").Set(float64(fresh))

	p := &LocalProvider{Gatherer: reg, Now: func() time.Time { return now }}
	got, _ := p.List(context.Background())
	for _, a := range got {
		if a.Alertname == "BackupOverdue" {
			t.Errorf("fresh run should not be overdue: %+v", a)
		}
	}
}

func TestLocalProvider_DumpSizeCollapsedAndDestFailing(t *testing.T) {
	reg, ms := fixedRegistry(t)
	ms.sizeRatio.WithLabelValues("orders").Set(0.2) // 20% — collapsed
	ms.destFailed.WithLabelValues("orders", "sftp-offsite").Set(1)
	ms.lastStatus.WithLabelValues("orders").Set(0) // also failed

	p := NewLocalProvider(reg)
	got, _ := p.List(context.Background())

	want := map[string]bool{
		"BackupDumpSizeCollapsed":  true,
		"BackupDestinationFailing": true,
		"BackupLastRunFailed":      true,
	}
	for _, a := range got {
		delete(want, a.Alertname)
	}
	if len(want) != 0 {
		t.Errorf("missing alerts: %v (got %+v)", want, got)
	}
}

func TestLocalProvider_AlertsSortedBySeverity(t *testing.T) {
	reg, ms := fixedRegistry(t)
	ms.sizeRatio.WithLabelValues("a").Set(0.1)         // critical
	ms.lastStatus.WithLabelValues("b").Set(0)          // warning
	ms.schemaChanged.WithLabelValues("c").Set(1)       // info
	ms.anomalies.WithLabelValues("d").Set(3)           // warning

	p := NewLocalProvider(reg)
	got, _ := p.List(context.Background())

	if len(got) < 4 {
		t.Fatalf("want 4 alerts, got %d", len(got))
	}
	prev := -1
	for _, a := range got {
		r := severityRank(a.Severity)
		if r < prev {
			t.Errorf("alerts not sorted by severity: %+v", got)
			break
		}
		prev = r
	}
}

func TestLocalProvider_NilGathererReturnsError(t *testing.T) {
	p := &LocalProvider{}
	if _, err := p.List(context.Background()); err == nil {
		t.Error("expected error when gatherer not set")
	}
}
