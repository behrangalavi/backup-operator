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
	storageFactory "backup-operator/storage/factory"
	"backup-operator/verifier"

	"github.com/go-logr/logr"
)

func (p *Pipeline) runRestoreVerifier(
	ctx context.Context,
	src *secrets.Source,
	dumpFile string,
	id *crypto.EphemeralIdentity,
	preStats *dumper.Stats,
	dumpCounts map[string]int64,
	log logr.Logger,
) *meta.RestoreVerificationResult {
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
	res, err := v.Verify(ctx, verifier.Input{
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
			st, err := storageFactory.NewStorage(d.StorageType, d.Name, d.Data, log)
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
