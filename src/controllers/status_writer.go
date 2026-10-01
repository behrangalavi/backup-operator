package controllers

import (
	"context"
	"sync"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"backup-operator/api/v1alpha1"
	"backup-operator/internal/meta"
	"backup-operator/internal/redact"
	"backup-operator/internal/scheduler"
	"backup-operator/internal/secrets"
)

// destRun is what one destination's latest meta lookup yielded for a
// source, kept so the status writer can fill per-destination fields.
type destRun struct {
	ts time.Time
	m  *meta.MetaFile
}

// destProbes aggregates, across every source in one refresh tick, whether
// each destination could be read. A destination is Reachable when no
// source's lookup against it ended in metaError this tick. The first error
// message wins — one is enough for an operator to start debugging.
type destProbes struct {
	mu   sync.Mutex
	seen map[string]bool
	errs map[string]string
}

func newDestProbes() *destProbes {
	return &destProbes{seen: map[string]bool{}, errs: map[string]string{}}
}

func (p *destProbes) record(name string, outcome metaOutcome, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen[name] = true
	if outcome != metaError {
		return
	}
	if _, ok := p.errs[name]; ok {
		return
	}
	msg := redact.StorageError(err)
	if msg == "" {
		msg = "storage unreachable or unreadable"
	}
	p.errs[name] = msg
}

// writeDestinationStatuses sets the Reachable condition on every
// BackupDestination probed this tick. Destinations no source fans out to
// are left at Unknown — nothing touched them, so claiming either way would
// be a guess.
func (r *MetricsRefresher) writeDestinationStatuses(ctx context.Context, res *BackupListResult, probes *destProbes) {
	probes.mu.Lock()
	seen := make(map[string]bool, len(probes.seen))
	for k, v := range probes.seen {
		seen[k] = v
	}
	errs := make(map[string]string, len(probes.errs))
	for k, v := range probes.errs {
		errs[k] = v
	}
	probes.mu.Unlock()

	for name, bd := range res.DestObjects {
		cond := metav1.Condition{
			Type:               v1alpha1.ConditionReachable,
			Status:             metav1.ConditionUnknown,
			Reason:             "NoSources",
			Message:            "no source currently fans out to this destination",
			ObservedGeneration: bd.Generation,
		}
		lastErr := ""
		if seen[name] {
			if msg, failed := errs[name]; failed {
				cond.Status = metav1.ConditionFalse
				cond.Reason = "ProbeFailed"
				cond.Message = msg
				lastErr = msg
			} else {
				cond.Status = metav1.ConditionTrue
				cond.Reason = "ProbeSucceeded"
				cond.Message = "destination listed successfully"
			}
		}
		err := updateDestinationStatus(ctx, r.Client, client.ObjectKeyFromObject(bd), func(st *v1alpha1.BackupDestinationStatus) {
			st.ObservedGeneration = bd.Generation
			st.LastError = lastErr
			apimeta.SetStatusCondition(&st.Conditions, cond)
		})
		if err != nil {
			r.Logger.V(1).Info("destination status update failed", "destination", name, "err", err.Error())
		}
	}
}

// writeSourceStatus mirrors what refreshSource just pushed into Prometheus
// onto the BackupSource's status subresource, so `kubectl get backupsources`
// answers "when did this last succeed" without a metrics stack. Status is
// best-effort: a failed write is logged and the next tick retries.
func (r *MetricsRefresher) writeSourceStatus(ctx context.Context, bs *v1alpha1.BackupSource, src *secrets.Source,
	newest *meta.MetaFile, newestTS time.Time, success *meta.MetaFile, successTS time.Time, runs map[string]destRun) {

	materialised := scheduler.ApplyJitter(src.Schedule, src.JitterMinutes, src.ObjectName)
	next := nextScheduleTime(materialised, src.Suspended, time.Now())

	err := updateSourceStatus(ctx, r.Client, client.ObjectKeyFromObject(bs), func(st *v1alpha1.BackupSourceStatus) {
		st.NextScheduleTime = next
		if newest == nil {
			return
		}
		st.LastRunTime = timePtr(newestTS)
		if newest.IsFailure() {
			st.LastRunStatus = v1alpha1.RunStatusFailed
			st.LastRunError = redact.String(newest.Error)
		} else {
			st.LastRunStatus = v1alpha1.RunStatusSuccess
			st.LastRunError = ""
		}
		if newest.Report != nil {
			st.LastRunAnomalies = int32(len(newest.Report.Anomalies))
		}
		if success != nil {
			st.LastSuccessTime = timePtr(successTS)
			st.LastDumpSizeBytes = success.EncryptedSizeBytes
			if !success.SchemaChangedAt.IsZero() {
				st.SchemaChangedAt = timePtr(success.SchemaChangedAt)
			}
		}

		// Per-destination: the newest meta carries each destination's
		// upload outcome; the per-destination probe tells us when that
		// destination last held a successful run.
		st.Destinations = st.Destinations[:0]
		for _, d := range newest.Destinations {
			ds := v1alpha1.DestinationRunStatus{
				Name:   d.Name,
				Status: d.Status,
				Error:  redact.String(d.Error),
			}
			if run, ok := runs[d.Name]; ok && run.m != nil && !run.m.IsFailure() {
				ds.LastSuccessTime = timePtr(run.ts)
			}
			st.Destinations = append(st.Destinations, ds)
		}
		if len(st.Destinations) == 0 {
			st.Destinations = nil
		}

		runCond := metav1.Condition{
			Type:               v1alpha1.ConditionLastRunSucceeded,
			Status:             metav1.ConditionTrue,
			Reason:             "RunSucceeded",
			Message:            "run " + newest.Timestamp + " completed",
			ObservedGeneration: bs.Generation,
		}
		if newest.IsFailure() {
			runCond.Status = metav1.ConditionFalse
			runCond.Reason = "RunFailed"
			runCond.Message = st.LastRunError
			if runCond.Message == "" {
				runCond.Message = "run " + newest.Timestamp + " failed"
			}
		}
		apimeta.SetStatusCondition(&st.Conditions, runCond)

		// Restore verification: prefer the success pivot so a transient
		// failed run doesn't erase the last verdict — same rule as the gauge.
		rvSource := newest
		if success != nil && success.RestoreVerification != nil {
			rvSource = success
		}
		if rvSource.RestoreVerification != nil {
			rv := rvSource.RestoreVerification
			st.RestoreVerification = &v1alpha1.RestoreVerificationStatus{
				Mode:    rv.Mode,
				Verdict: rv.Verdict,
				Error:   redact.String(rv.Error),
			}
			if !rv.CompletedAt.IsZero() {
				st.RestoreVerification.CompletedAt = timePtr(rv.CompletedAt)
			}
			cond := metav1.Condition{
				Type:               v1alpha1.ConditionVerified,
				Status:             metav1.ConditionFalse,
				Reason:             "Verdict" + capitalize(rv.Verdict),
				Message:            "restore verification (" + rv.Mode + ") verdict: " + rv.Verdict,
				ObservedGeneration: bs.Generation,
			}
			if rv.Verdict == meta.VerificationMatch {
				cond.Status = metav1.ConditionTrue
			}
			apimeta.SetStatusCondition(&st.Conditions, cond)
		}
	})
	if err != nil {
		r.Logger.V(1).Info("source status update failed", "source", bs.Name, "err", err.Error())
	}
}

func timePtr(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	mt := metav1.NewTime(t.UTC())
	return &mt
}

// capitalize upper-cases the first byte for CamelCase condition reasons
// ("match" → "Match"). Verdicts are short ASCII enums, so bytes suffice.
func capitalize(s string) string {
	if s == "" {
		return "Unknown"
	}
	if s[0] >= 'a' && s[0] <= 'z' {
		return string(s[0]-'a'+'A') + s[1:]
	}
	return s
}
