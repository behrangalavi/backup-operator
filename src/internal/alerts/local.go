package alerts

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// LocalProvider re-evaluates the chart's PrometheusRule conditions against
// the operator's own metric registry. It is the no-Prometheus-configured
// fallback: zero external setup, alerts appear immediately rather than after
// the rule's "for:" duration. We document the difference in the Source field
// so the UI can communicate that this is not the audit-grade path.
//
// Conditions kept in sync with charts/backup-operator/values.yaml. When that
// file changes, this evaluator must change too — there is no shared source
// of truth because we deliberately want both paths independent (this one
// serves users who never deploy Prometheus at all).
type LocalProvider struct {
	Gatherer prometheus.Gatherer
	Now      func() time.Time // override in tests
}

func NewLocalProvider(g prometheus.Gatherer) *LocalProvider {
	return &LocalProvider{Gatherer: g, Now: time.Now}
}

// targetState collects every metric value we need per target/destination so
// we can evaluate all six rules in one pass over the gathered families.
type targetState struct {
	lastSuccessTs       map[string]float64 // dest → unix ts
	destinationFailed   map[string]float64 // dest → 0/1
	dumpSizeChangeRatio float64
	dumpSizeChangeKnown bool
	schemaChanged       float64
	schemaChangedKnown  bool
	lastRunStatus       float64
	lastRunStatusKnown  bool
	lastRunAnomalies    float64
	anomaliesKnown      bool
}

func (p *LocalProvider) List(ctx context.Context) ([]Alert, error) {
	if p.Gatherer == nil {
		return nil, fmt.Errorf("local provider has no gatherer (metrics not registered)")
	}
	families, err := p.Gatherer.Gather()
	if err != nil {
		return nil, fmt.Errorf("gather: %w", err)
	}

	states := map[string]*targetState{}
	getState := func(target string) *targetState {
		s, ok := states[target]
		if !ok {
			s = &targetState{
				lastSuccessTs:     map[string]float64{},
				destinationFailed: map[string]float64{},
			}
			states[target] = s
		}
		return s
	}

	for _, fam := range families {
		for _, m := range fam.Metric {
			labels := labelMap(m.Label)
			target := labels["target"]
			if target == "" {
				continue
			}
			val := gaugeValue(m)
			switch fam.GetName() {
			case "backup_operator_last_success_timestamp_seconds":
				getState(target).lastSuccessTs[labels["destination"]] = val
			case "backup_operator_destination_failed":
				getState(target).destinationFailed[labels["destination"]] = val
			case "backup_operator_dump_size_change_ratio":
				s := getState(target)
				s.dumpSizeChangeRatio = val
				s.dumpSizeChangeKnown = true
			case "backup_operator_schema_changed":
				s := getState(target)
				s.schemaChanged = val
				s.schemaChangedKnown = true
			case "backup_operator_last_run_status":
				s := getState(target)
				s.lastRunStatus = val
				s.lastRunStatusKnown = true
			case "backup_operator_last_run_anomalies":
				s := getState(target)
				s.lastRunAnomalies = val
				s.anomaliesKnown = true
			}
		}
	}

	now := p.Now()
	var out []Alert
	for target, s := range states {
		// 1. BackupOverdue — newest success across destinations is older
		//    than 36h. We use the newest because fan-out means at least one
		//    destination should have the recent run.
		var newestSuccess float64
		for _, ts := range s.lastSuccessTs {
			if ts > newestSuccess {
				newestSuccess = ts
			}
		}
		if newestSuccess > 0 && now.Unix()-int64(newestSuccess) > int64(86400*1.5) {
			out = append(out, Alert{
				Alertname:   "BackupOverdue",
				Target:      target,
				Severity:    "warning",
				State:       "firing",
				ActiveSince: time.Unix(int64(newestSuccess), 0).Add(36 * time.Hour),
				Summary:     fmt.Sprintf("Backup target %s hasn't succeeded in over 36h", target),
				Source:      "local",
			})
		}

		// 2. BackupDestinationFailing — per destination
		for dest, failed := range s.destinationFailed {
			if failed == 1 {
				out = append(out, Alert{
					Alertname:   "BackupDestinationFailing",
					Target:      target,
					Destination: dest,
					Severity:    "warning",
					State:       "firing",
					ActiveSince: now,
					Summary:     fmt.Sprintf("Backup target %s failing to %s", target, dest),
					Source:      "local",
				})
			}
		}

		// 3. BackupDumpSizeCollapsed
		if s.dumpSizeChangeKnown && s.dumpSizeChangeRatio > 0 && s.dumpSizeChangeRatio < 0.5 {
			out = append(out, Alert{
				Alertname:   "BackupDumpSizeCollapsed",
				Target:      target,
				Severity:    "critical",
				State:       "firing",
				ActiveSince: now,
				Summary: fmt.Sprintf(
					"Backup %s shrunk to %.0f%% of previous size — possible data loss",
					target, s.dumpSizeChangeRatio*100,
				),
				Source: "local",
			})
		}

		// 4. BackupSchemaChanged — informational, not a failure
		if s.schemaChangedKnown && s.schemaChanged == 1 {
			out = append(out, Alert{
				Alertname:   "BackupSchemaChanged",
				Target:      target,
				Severity:    "info",
				State:       "firing",
				ActiveSince: now,
				Summary:     fmt.Sprintf("Schema changed for backup target %s", target),
				Source:      "local",
			})
		}

		// 5. BackupAnomaliesAppearing
		if s.anomaliesKnown && s.lastRunAnomalies > 0 {
			out = append(out, Alert{
				Alertname:   "BackupAnomaliesAppearing",
				Target:      target,
				Severity:    "warning",
				State:       "firing",
				ActiveSince: now,
				Summary: fmt.Sprintf(
					"Analyzer reported %.0f anomalies in the last run of %s",
					s.lastRunAnomalies, target,
				),
				Source: "local",
			})
		}

		// 6. BackupLastRunFailed
		if s.lastRunStatusKnown && s.lastRunStatus == 0 {
			out = append(out, Alert{
				Alertname:   "BackupLastRunFailed",
				Target:      target,
				Severity:    "warning",
				State:       "firing",
				ActiveSince: now,
				Summary:     fmt.Sprintf("Most recent backup run for %s did not produce a usable artifact", target),
				Source:      "local",
			})
		}
	}

	// Stable order: severity (critical → warning → info), then alertname,
	// then target. UIs that paginate or hash this list need determinism.
	sort.Slice(out, func(i, j int) bool {
		if r := severityRank(out[i].Severity) - severityRank(out[j].Severity); r != 0 {
			return r < 0
		}
		if out[i].Alertname != out[j].Alertname {
			return out[i].Alertname < out[j].Alertname
		}
		return out[i].Target < out[j].Target
	})
	return out, nil
}

func severityRank(s string) int {
	switch s {
	case "critical":
		return 0
	case "warning":
		return 1
	case "info":
		return 2
	}
	return 3
}

func labelMap(pairs []*dto.LabelPair) map[string]string {
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		out[p.GetName()] = p.GetValue()
	}
	return out
}

func gaugeValue(m *dto.Metric) float64 {
	if g := m.Gauge; g != nil {
		return g.GetValue()
	}
	return 0
}
