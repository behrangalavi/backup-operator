package backup

import (
	"bytes"
	"context"
	"sync"
	"time"

	"backup-operator/crypto"
	"backup-operator/dumper"
	"backup-operator/internal/meta"
	"backup-operator/internal/safe"
	"backup-operator/internal/secrets"
	"backup-operator/metrics"
	"backup-operator/verifier"

	"github.com/go-logr/logr"
)

// verifierBudget returns how long the restore verifier may run: at most 70% of
// the run context's remaining time, so meta.json upload + retention always keep
// headroom and a slow verifier can't starve them. Falls back to 30m when the
// context carries no deadline (shouldn't happen for a real run, but keeps the
// verifier bounded in tests / unusual setups).
func verifierBudget(ctx context.Context) time.Duration {
	const fallback = 30 * time.Minute
	dl, ok := ctx.Deadline()
	if !ok {
		return fallback
	}
	remaining := time.Until(dl)
	if remaining <= 0 {
		// No time left; hand the verifier a tiny budget so it fails fast with a
		// timeout result rather than a negative/expired context.
		return time.Second
	}
	budget := time.Duration(float64(remaining) * 0.7)
	if budget > fallback {
		budget = fallback
	}
	return budget
}

func (p *Pipeline) runRestoreVerifier(
	ctx context.Context,
	src *secrets.Source,
	dumpFile string,
	id *crypto.EphemeralIdentity,
	preStats *dumper.Stats,
	dumpCounts map[string]int64,
	log logr.Logger,
) *meta.RestoreVerificationResult {
	// Restore verification is observability, NOT a gate (§14): it must never
	// turn a successful backup into a failed run. It runs after the dump has
	// uploaded but before meta.json, on the same run context — so a slow
	// verifier (Phase-2 full restore of a big DB) that eats the remaining
	// RUN_TIMEOUT would cancel ctx, fail the subsequent uploadMeta on every
	// destination, and trip the phantom-backup guard into failing the whole
	// run (worker exit 1, K8s re-dumps). Give the verifier its own bounded
	// sub-context so its deadline can never cascade into uploadMeta; if it
	// blows the budget it yields a skipped/timeout result and the run still
	// completes successfully.
	vctx, cancel := context.WithTimeout(ctx, verifierBudget(ctx))
	defer cancel()

	mode := src.RestoreVerificationMode
	v, err := p.verifierFactory(mode, src.DBType, log)
	if err != nil {
		log.Error(err, "restore-verifier factory failed", "mode", mode)
		return verifier.FailureResult(mode, time.Now().UTC(), err, id.RecipientFingerprint())
	}
	if v == nil {
		// "off" should not reach here (we already gated on mode != ""),
		// but defensively short-circuit.
		return nil
	}
	started := time.Now().UTC()
	res, err := v.Verify(vctx, verifier.Input{
		Source:      src,
		DumpPath:    dumpFile,
		Identity:    id,
		PreStats:    preStats,
		DumpRows:    dumpCounts,
		Compression: src.Compression,
		StartedAt:   started,
		Logger:      log,
		Spawner:     p.spawner,
		Namespace:   p.namespace,
		OwnerRef:    p.ownerRef,
	})
	if err != nil {
		// Budget exhaustion (our sub-context timed out while the run's parent
		// context is still alive) is NOT a verification failure — the dump may
		// well be restorable, the verifier just didn't fit in its time slice.
		// Returning a FailureResult here would map to Verdict=Skipped, which
		// §14 treats like a hard mismatch → a false critical
		// BackupRestoreVerificationFailed for a merely-slow (large) restore,
		// re-introducing the very Skipped→false-critical class the budget was
		// added around. Return nil so the caller carries the previous verdict
		// forward (preserving the interval clock too). A parent-ctx timeout is
		// left as a failure — the whole run is aborting anyway.
		if vctx.Err() != nil && ctx.Err() == nil {
			log.Info("restore verifier exceeded its time budget; keeping previous verdict", "mode", mode)
			return nil
		}
		log.Error(err, "restore-verifier hard failure", "mode", mode)
		return verifier.FailureResult(mode, started, err, id.RecipientFingerprint())
	}
	log.Info("restore-verification result",
		"mode", res.Mode,
		"verdict", res.Verdict,
		"duration_seconds", res.DurationSeconds,
		"summary", res.Summary,
	)
	// Worker-side metrics: histogram is process-local (Prometheus can't
	// scrape the short-lived pod), but emit anyway for symmetry; the
	// gauges that DO surface to Prometheus are reconstructed by the
	// MetricsRefresher reading meta.json.
	metrics.ObserveRestoreVerificationDuration(src.TargetName, res.Mode, time.Duration(res.DurationSeconds*float64(time.Second)))
	return res
}

// recordFailure best-effort uploads a failure-meta sidecar to every
// destination so the UI surfaces the failed run. Upload errors are
// swallowed: the run is already failing — masking the original error with
// a secondary one would obscure the actual cause in logs.
func (p *Pipeline) recordFailure(
	ctx context.Context,
	dests []*secrets.Destination,
	src *secrets.Source,
	timestamp, phase string,
	runStart time.Time,
	runErr error,
	log logr.Logger,
) {
	if len(dests) == 0 {
		return
	}

	// Use a detached context with a short timeout: the parent ctx may already
	// be cancelled (e.g. after a context-deadline exceeded), but we still want
	// to persist the failure-meta so the UI surfaces the failed run.
	uploadCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	body, marshalErr := failureMetaJSON(src, timestamp, phase, runStart, runErr)
	if marshalErr != nil {
		log.Error(marshalErr, "failure-meta marshal failed; uploading fallback meta", "target", src.TargetName, "phase", phase)
		body = fallbackMetaJSON(src.TargetName, timestamp, src.DBType, phase, marshalErr)
	}
	metaPath := buildObjectPath(src.TargetName, timestamp, "meta.json")

	var wg sync.WaitGroup
	for _, dest := range dests {
		wg.Add(1)
		go func(d *secrets.Destination) {
			defer wg.Done()
			defer safe.Goroutine(log, "failure-meta", d.Name)
			st, err := p.getStorage(d)
			if err != nil {
				log.V(1).Info("failure-meta: init storage failed", "destination", d.Name, "err", err.Error())
				return
			}
			if err := st.Upload(uploadCtx, metaPath, bytes.NewReader(body)); err != nil {
				log.V(1).Info("failure-meta: upload failed", "destination", d.Name, "err", err.Error())
				return
			}
			log.Info("failure-meta written", "destination", d.Name, "phase", phase)
		}(dest)
	}
	wg.Wait()
}
