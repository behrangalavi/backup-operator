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
// serves users who never deploy Prometheus at all). Every problem-signalling
// rule is mirrored here; only BackupSucceeded is intentionally omitted — it is
// a positive heartbeat, not a condition the UI should surface as "firing". The
// two restore-verification rules carry their `mode` label in the summary text
// (the Alert schema has no mode field), matching how the PrometheusProvider
// surfaces the rule's annotation.
type LocalProvider struct {
	Gatherer prometheus.Gatherer
	Now      func() time.Time // override in tests
}

func NewLocalProvider(g prometheus.Gatherer) *LocalProvider {
	return &LocalProvider{Gatherer: g, Now: time.Now}
}

// targetState collects every metric value we need per target/destination so
// we can evaluate all rules in one pass over the gathered families.
type targetState struct {
	lastSuccessTs       map[string]float64 // dest → unix ts
	destinationFailed   map[string]float64 // dest → 0/1
	storageScrubPassed  map[string]float64 // dest → 0/1 (only present after a scrub ran)
	retentionPassed     map[string]float64 // dest → 0/1 (only present after a sweep ran)
	restoreVerifPassed  map[string]float64 // mode → 0/1 (only present after a verifier ran)
	restoreVerifLastTs  map[string]float64 // mode → unix ts (only present after a verifier ran)
	dumpSizeChangeRatio float64
	dumpSizeChangeKnown bool
	schemaChanged       float64
	schemaChangedKnown  bool
	charsetChanged      float64
	charsetChangedKnown bool
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
				lastSuccessTs:      map[string]float64{},
				destinationFailed:  map[string]float64{},
				storageScrubPassed: map[string]float64{},
				retentionPassed:    map[string]float64{},
				restoreVerifPassed: map[string]float64{},
				restoreVerifLastTs: map[string]float64{},
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
			case "backup_operator_storage_scrub_passed":
				getState(target).storageScrubPassed[labels["destination"]] = val
			case "backup_operator_retention_last_status":
				getState(target).retentionPassed[labels["destination"]] = val
			case "backup_operator_restore_verification_passed":
				getState(target).restoreVerifPassed[labels["mode"]] = val
			case "backup_operator_restore_verification_last_timestamp_seconds":
				getState(target).restoreVerifLastTs[labels["mode"]] = val
			case "backup_operator_dump_size_change_ratio":
				s := getState(target)
				s.dumpSizeChangeRatio = val
				s.dumpSizeChangeKnown = true
			case "backup_operator_schema_changed":
				s := getState(target)
				s.schemaChanged = val
				s.schemaChangedKnown = true
			case "backup_operator_charset_changed":
				s := getState(target)
				s.charsetChanged = val
				s.charsetChangedKnown = true
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

		// 3. BackupDumpSizeCollapsed. Mirror the Prometheus rule's bare
		// `< 0.5` — the dumpSizeChangeKnown flag already guards "gauge unset",
		// so the old extra `> 0` was redundant and wrongly suppressed a total
		// collapse to zero bytes (ratio 0), which Prometheus would fire on.
		if s.dumpSizeChangeKnown && s.dumpSizeChangeRatio < 0.5 {
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

		// 4b. BackupCharsetChanged — warning. utf8 → utf8mb4 drift silently
		// truncates 4-byte chars on restore; treat with more weight than
		// schema drift.
		if s.charsetChangedKnown && s.charsetChanged == 1 {
			out = append(out, Alert{
				Alertname:   "BackupCharsetChanged",
				Target:      target,
				Severity:    "warning",
				State:       "firing",
				ActiveSince: now,
				Summary:     fmt.Sprintf("Database charset/collation changed for backup target %s", target),
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

		// 5b. BackupStorageCorrupted — per destination. Critical because the
		// stored dump's bytes no longer match the SHA256 in meta.json: the
		// artifact is unrecoverable, period.
		for dest, passed := range s.storageScrubPassed {
			if passed == 0 {
				out = append(out, Alert{
					Alertname:   "BackupStorageCorrupted",
					Target:      target,
					Destination: dest,
					Severity:    "critical",
					State:       "firing",
					ActiveSince: now,
					Summary:     fmt.Sprintf("Storage scrub failed for %s on %s — dump SHA256 does not match meta.json", target, dest),
					Source:      "local",
				})
			}
		}

		// 5c. BackupRetentionFailing — per destination. The PrometheusRule
		// has a 24h "for:" debounce because retention failures aren't
		// instantly fatal; the local heuristic doesn't honor durations
		// (documented constraint), so it fires immediately. Operators
		// see "in-UI" but not "via Alertmanager" until 24h elapsed.
		for dest, passed := range s.retentionPassed {
			if passed == 0 {
				out = append(out, Alert{
					Alertname:   "BackupRetentionFailing",
					Target:      target,
					Destination: dest,
					Severity:    "warning",
					State:       "firing",
					ActiveSince: now,
					Summary:     fmt.Sprintf("Retention failing for %s on %s — storage will eventually fill", target, dest),
					Source:      "local",
				})
			}
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

		// 7. BackupRestoreVerificationFailed — per mode. The metric is absent
		// until a verifier has run at least once, so a present value of 0
		// unambiguously means "the most recent verifier run did not produce a
		// `match` verdict" (mismatch or skipped). Critical: the encrypted
		// artifact could not be proven decryptable+parseable. mode is folded
		// into the summary (matching the PrometheusRule annotation) since the
		// Alert schema carries no mode field.
		for mode, passed := range s.restoreVerifPassed {
			if passed == 0 {
				out = append(out, Alert{
					Alertname:   "BackupRestoreVerificationFailed",
					Target:      target,
					Severity:    "critical",
					State:       "firing",
					ActiveSince: now,
					Summary:     fmt.Sprintf("Restore-verification failed for %s (mode=%s) — encrypted dump did not decrypt+parse cleanly", target, mode),
					Source:      "local",
				})
			}
		}

		// 8. BackupRestoreVerificationStale — per mode. Verification is
		// configured but the most recent completion is older than 14 days, so
		// the "a backup nobody has proven restorable" window has opened.
		for mode, ts := range s.restoreVerifLastTs {
			if ts > 0 && now.Unix()-int64(ts) > int64(86400*14) {
				out = append(out, Alert{
					Alertname:   "BackupRestoreVerificationStale",
					Target:      target,
					Severity:    "warning",
					State:       "firing",
					ActiveSince: time.Unix(int64(ts), 0).Add(14 * 24 * time.Hour),
					Summary:     fmt.Sprintf("Restore-verification for %s (mode=%s) hasn't run in over 14 days", target, mode),
					Source:      "local",
				})
			}
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
