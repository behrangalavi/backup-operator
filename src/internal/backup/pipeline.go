package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"backup-operator/analyzer"
	"backup-operator/crypto"
	"backup-operator/dumper"
	dumperFactory "backup-operator/dumper/factory"
	"backup-operator/internal/meta"
	"backup-operator/internal/secrets"
	"backup-operator/metrics"
	"backup-operator/storage"
	storageFactory "backup-operator/storage/factory"

	"github.com/go-logr/logr"
)

// EventEmitter abstracts Kubernetes event recording so the pipeline stays
// testable without a real API server. The worker injects an implementation
// backed by record.EventRecorder; tests can use NoopEventEmitter.
type EventEmitter interface {
	Emit(eventType, reason, message string)
}

// NoopEventEmitter silently drops events — used in tests and the restore CLI.
type NoopEventEmitter struct{}

func (NoopEventEmitter) Emit(string, string, string) {}

// Pipeline runs one backup of one Source to N Destinations:
//   1. CollectStats from the live DB (best-effort; missing stats just skip the analyzer step).
//   2. Dump → gzip → age → temp file (single dump regardless of N destinations).
//   3. Fan out the temp file to all destinations in parallel.
//   4. Write a sidecar meta JSON (unencrypted) with stats + analyzer report.
//   5. Compare with previous meta to populate analyzer metrics.
//   6. Apply retention policy per destination (best-effort, never fails the run).
type Pipeline struct {
	encryptor      crypto.Encryptor
	analyzer       analyzer.Analyzer
	tempDir        string
	logger         logr.Logger
	destProvider   DestinationProvider
	defaults       RetentionPolicy
	events         EventEmitter
	maxConcurrency int
}

// DestinationProvider returns the current set of destinations at run time.
// Implemented by the controller cache so we always pick up new destinations.
type DestinationProvider interface {
	Destinations() []*secrets.Destination
}

func NewPipeline(
	enc crypto.Encryptor,
	an analyzer.Analyzer,
	tempDir string,
	dp DestinationProvider,
	defaults RetentionPolicy,
	logger logr.Logger,
) *Pipeline {
	return &Pipeline{
		encryptor:      enc,
		analyzer:       an,
		tempDir:        tempDir,
		logger:         logger,
		destProvider:   dp,
		defaults:       defaults,
		events:         NoopEventEmitter{},
		maxConcurrency: defaultMaxConcurrency,
	}
}

// NewPipelineWithEvents creates a pipeline that emits Kubernetes events for
// audit-trail compliance. Used by the worker binary.
func NewPipelineWithEvents(
	enc crypto.Encryptor,
	an analyzer.Analyzer,
	tempDir string,
	dp DestinationProvider,
	defaults RetentionPolicy,
	logger logr.Logger,
	events EventEmitter,
) *Pipeline {
	return &Pipeline{
		encryptor:      enc,
		analyzer:       an,
		tempDir:        tempDir,
		logger:         logger,
		destProvider:   dp,
		defaults:       defaults,
		events:         events,
		maxConcurrency: defaultMaxConcurrency,
	}
}

// resolvePolicy turns the source's annotation values + global defaults into
// the concrete RetentionPolicy used for this run. -1 on either field means
// "annotation absent — use the global default"; an explicit 0 from the user
// disables retention even when the global default would prune.
func (p *Pipeline) resolvePolicy(src *secrets.Source) RetentionPolicy {
	policy := p.defaults
	if src.RetentionDays >= 0 {
		policy.Days = src.RetentionDays
	}
	if src.MinKeep >= 0 {
		policy.MinKeep = src.MinKeep
	}
	return policy
}

// analyzerForSource returns a per-source analyzer with thresholds from
// annotations, falling back to the pipeline's default analyzer when both
// thresholds are absent (-1).
func (p *Pipeline) analyzerForSource(src *secrets.Source) analyzer.Analyzer {
	if src.RowDropThreshold < 0 && src.SizeDropThreshold < 0 {
		return p.analyzer
	}
	return analyzer.NewAnalyzerWithThresholds(src.RowDropThreshold, src.SizeDropThreshold)
}

// Run executes a full backup. Errors during destination uploads are reported
// per-destination via metrics; the function returns nil unless the dump itself
// fails or no destination accepts the artifact.
//
// Failed runs are persisted as failure-meta sidecars to every reachable
// destination so the UI can list them next to successful runs. Best-effort:
// failure-meta upload errors are logged but never alter the returned error.
func (p *Pipeline) Run(ctx context.Context, src *secrets.Source) error {
	log := p.logger.WithValues("target", src.TargetName, "db_type", src.DBType)
	runStart := time.Now()
	defer func() { metrics.ObserveRunDuration(src.TargetName, src.DBType, time.Since(runStart)) }()
	timestamp := runStart.UTC().Format("20060102T150405Z")

	p.events.Emit("Normal", "BackupStarted",
		fmt.Sprintf("Backup started for target %s (db=%s)", src.TargetName, src.DBType))

	// Resolve destinations up-front so we can persist a failure-meta even
	// when the dump itself fails.
	dests := secrets.FilterDestinations(src, p.destProvider.Destinations())

	d, err := dumperFactory.NewDumper(src.DBType, src.Config, log)
	if err != nil {
		metrics.SetLastRunStatus(src.TargetName, false)
		p.events.Emit("Warning", "BackupFailed",
			fmt.Sprintf("Backup failed for target %s in phase dumper-init: %v", src.TargetName, err))
		p.recordFailure(ctx, dests, src, timestamp, "dumper-init", err, log)
		return fmt.Errorf("dumper: %w", err)
	}

	var stats *dumper.Stats
	if src.AnalyzerEnabled {
		s, statsErr := d.CollectStats(ctx)
		if statsErr != nil {
			log.V(1).Info("stats collection skipped", "reason", statsErr.Error())
		} else {
			stats = s
		}
	} else {
		log.V(1).Info("analyzer disabled by annotation; skipping stats collection")
	}

	if err := os.MkdirAll(p.tempDir, 0o755); err != nil {
		metrics.SetLastRunStatus(src.TargetName, false)
		p.events.Emit("Warning", "BackupFailed",
			fmt.Sprintf("Backup failed for target %s in phase temp-dir: %v", src.TargetName, err))
		p.recordFailure(ctx, dests, src, timestamp, "temp-dir", err, log)
		return fmt.Errorf("create temp dir: %w", err)
	}
	dumpFile := path.Join(p.tempDir, fmt.Sprintf("%s-%s.sql.gz.age", src.TargetName, timestamp))

	dumpStart := time.Now()
	encryptedSize, sha256sum, err := p.dumpToFile(ctx, d, dumpFile)
	dumpDuration := time.Since(dumpStart)
	metrics.ObserveDumpDuration(src.TargetName, src.DBType, dumpDuration)

	if err != nil {
		metrics.SetLastRunStatus(src.TargetName, false)
		_ = os.Remove(dumpFile)
		p.events.Emit("Warning", "BackupFailed",
			fmt.Sprintf("Backup failed for target %s in phase dump: %v", src.TargetName, err))
		p.recordFailure(ctx, dests, src, timestamp, "dump", err, log)
		return fmt.Errorf("dump: %w", err)
	}
	defer func() { _ = os.Remove(dumpFile) }()

	if encryptedSize == 0 {
		metrics.SetLastRunStatus(src.TargetName, false)
		emptyErr := errors.New("dump produced zero bytes")
		p.events.Emit("Warning", "BackupFailed",
			fmt.Sprintf("Backup failed for target %s: dump produced zero bytes (possible empty database or dump tool misconfiguration)", src.TargetName))
		p.recordFailure(ctx, dests, src, timestamp, "dump-empty", emptyErr, log)
		return emptyErr
	}

	metrics.SetDumpSize(src.TargetName, encryptedSize)

	if len(dests) == 0 {
		metrics.SetLastRunStatus(src.TargetName, false)
		return errors.New("no destinations configured")
	}

	if err := ctx.Err(); err != nil {
		metrics.SetLastRunStatus(src.TargetName, false)
		p.events.Emit("Warning", "BackupFailed",
			fmt.Sprintf("Backup cancelled for target %s after dump phase: %v", src.TargetName, err))
		p.recordFailure(ctx, dests, src, timestamp, "cancelled", err, log)
		return fmt.Errorf("cancelled after dump: %w", err)
	}

	objectPath := buildObjectPath(src.TargetName, timestamp, "sql.gz.age")
	metaPath := buildObjectPath(src.TargetName, timestamp, "meta.json")

	var report *analyzer.Report
	if src.AnalyzerEnabled {
		prevStats, prevSize := p.loadPreviousStats(ctx, dests, src.TargetName)
		an := p.analyzerForSource(src)
		report = an.Compare(prevStats, stats, prevSize, encryptedSize)
		emitAnalyzerMetrics(src.TargetName, report)
	}

	metaStats := stats
	metaReport := report
	if src.AnonymizeTables {
		if stats != nil {
			metaStats = anonymizeStats(stats)
		}
		if report != nil {
			metaReport = anonymizeReport(report)
		}
	}

	// Phase 1: fan-out dumps to all destinations, collecting per-destination results.
	destResults := p.fanOutDumps(ctx, dests, src.TargetName, dumpFile, objectPath, log)
	successCount := 0
	for _, dr := range destResults {
		if dr.Status == meta.StatusSuccess {
			successCount++
		}
	}
	if successCount == 0 {
		metrics.SetLastRunStatus(src.TargetName, false)
		p.events.Emit("Warning", "BackupFailed",
			fmt.Sprintf("Backup failed for target %s: all %d destination uploads failed", src.TargetName, len(dests)))
		p.recordFailure(ctx, dests, src, timestamp, "upload", errors.New("all destination uploads failed"), log)
		return errors.New("all destination uploads failed")
	}

	// Phase 2: build meta with destination results, upload to successful destinations.
	metaBytes := metaJSON(src, metaStats, metaReport, encryptedSize, sha256sum, timestamp, destResults)
	p.uploadMeta(ctx, dests, destResults, metaPath, metaBytes, log)

	metrics.SetLastRunStatus(src.TargetName, true)
	p.events.Emit("Normal", "BackupCompleted",
		fmt.Sprintf("Backup completed for target %s (%d/%d destinations, %d bytes)",
			src.TargetName, successCount, len(dests), encryptedSize))
	log.Info("backup completed", "destinations_succeeded", successCount, "destinations_total", len(dests))

	// Retention runs after a successful upload so old artifacts are pruned
	// only once a fresh one is in place. Errors here do not fail the run.
	p.applyRetention(ctx, dests, src.TargetName, p.resolvePolicy(src), time.Now(), log)

	return nil
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

	body := failureMetaJSON(src, timestamp, phase, runErr)
	metaPath := buildObjectPath(src.TargetName, timestamp, "meta.json")

	var wg sync.WaitGroup
	for _, dest := range dests {
		wg.Add(1)
		go func(d *secrets.Destination) {
			defer wg.Done()
			defer recoverGoroutine(log, "failure-meta", d.Name)
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

func (p *Pipeline) dumpToFile(ctx context.Context, d dumper.Dumper, dumpFile string) (int64, string, error) {
	f, err := os.Create(dumpFile)
	if err != nil {
		return 0, "", fmt.Errorf("create temp dump: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	w := io.MultiWriter(f, h)

	enc, err := p.encryptor.Wrap(w)
	if err != nil {
		return 0, "", fmt.Errorf("encrypt wrap: %w", err)
	}
	gz := gzip.NewWriter(enc)

	if err := d.Dump(ctx, gz); err != nil {
		_ = gz.Close()
		_ = enc.Close()
		return 0, "", err
	}
	if err := gz.Close(); err != nil {
		_ = enc.Close()
		return 0, "", fmt.Errorf("gzip close: %w", err)
	}
	if err := enc.Close(); err != nil {
		return 0, "", fmt.Errorf("age close: %w", err)
	}

	if err := f.Sync(); err != nil {
		return 0, "", fmt.Errorf("fsync dump: %w", err)
	}

	info, err := f.Stat()
	if err != nil {
		return 0, "", err
	}
	return info.Size(), hex.EncodeToString(h.Sum(nil)), nil
}

const (
	uploadMaxRetries      = 3
	uploadBaseDelay       = 2 * time.Second
	defaultMaxConcurrency = 4
)

func (p *Pipeline) fanOutDumps(
	ctx context.Context,
	dests []*secrets.Destination,
	target, dumpFile, objectPath string,
	log logr.Logger,
) []meta.DestinationResult {
	results := make([]meta.DestinationResult, len(dests))
	var wg sync.WaitGroup
	sem := make(chan struct{}, p.maxConcurrency)
	for i, dest := range dests {
		results[i] = meta.DestinationResult{
			Name:        dest.Name,
			StorageType: dest.StorageType,
			Status:      meta.StatusFailed,
		}
		wg.Add(1)
		go func(idx int, d *secrets.Destination) {
			defer wg.Done()
			defer recoverGoroutine(log, "upload", d.Name)
			sem <- struct{}{}
			defer func() { <-sem }()
			err := p.uploadDumpWithRetry(ctx, d, target, dumpFile, objectPath, log)
			if err != nil {
				log.Error(err, "destination upload failed", "destination", d.Name)
				metrics.SetDestinationFailed(target, d.Name, true)
				results[idx].Error = err.Error()
				return
			}
			metrics.SetDestinationFailed(target, d.Name, false)
			metrics.SetLastSuccess(target, d.Name, time.Now())
			results[idx].Status = meta.StatusSuccess
		}(i, dest)
	}
	wg.Wait()
	return results
}

// uploadMeta uploads the meta.json sidecar to all destinations that had a
// successful dump upload. Best-effort: meta upload failures are logged but
// do not change the overall run result.
func (p *Pipeline) uploadMeta(
	ctx context.Context,
	dests []*secrets.Destination,
	results []meta.DestinationResult,
	metaPath string,
	metaBytes []byte,
	log logr.Logger,
) {
	var wg sync.WaitGroup
	for i, dest := range dests {
		if results[i].Status != meta.StatusSuccess {
			continue
		}
		wg.Add(1)
		go func(d *secrets.Destination) {
			defer wg.Done()
			defer recoverGoroutine(log, "meta-upload", d.Name)
			st, err := storageFactory.NewStorage(d.StorageType, d.Name, d.Data, p.logger)
			if err != nil {
				log.V(1).Info("meta upload: init storage failed", "destination", d.Name, "err", err.Error())
				return
			}
			if err := st.Upload(ctx, metaPath, bytes.NewReader(metaBytes)); err != nil {
				log.V(1).Info("meta upload failed", "destination", d.Name, "err", err.Error())
				return
			}
		}(dest)
	}
	wg.Wait()
}

// uploadDumpWithRetry wraps uploadDumpOne with exponential backoff for
// transient failures. Only RetryableError triggers a retry; PermanentError
// and other errors abort immediately.
func (p *Pipeline) uploadDumpWithRetry(
	ctx context.Context,
	d *secrets.Destination,
	target, dumpFile, objectPath string,
	log logr.Logger,
) error {
	var lastErr error
	for attempt := 0; attempt < uploadMaxRetries; attempt++ {
		lastErr = p.uploadDumpOne(ctx, d, target, dumpFile, objectPath)
		if lastErr == nil {
			return nil
		}

		var retryable *RetryableError
		if !errors.As(lastErr, &retryable) {
			return lastErr
		}

		if attempt < uploadMaxRetries-1 {
			delay := uploadBaseDelay * time.Duration(1<<uint(attempt))
			log.Info("retrying upload after transient failure",
				"destination", d.Name,
				"attempt", attempt+1,
				"delay", delay.String(),
				"err", lastErr.Error(),
			)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return lastErr
}

func (p *Pipeline) uploadDumpOne(
	ctx context.Context,
	d *secrets.Destination,
	target, dumpFile, objectPath string,
) error {
	st, err := storageFactory.NewStorage(d.StorageType, d.Name, d.Data, p.logger)
	if err != nil {
		return &PermanentError{Op: "init storage", Err: err}
	}

	start := time.Now()
	dump, err := os.Open(dumpFile)
	if err != nil {
		return fmt.Errorf("open dump: %w", err)
	}
	defer func() { _ = dump.Close() }()

	info, err := dump.Stat()
	if err != nil {
		return fmt.Errorf("stat dump: %w", err)
	}
	localSize := info.Size()

	if err := st.Upload(ctx, objectPath, dump); err != nil {
		return &RetryableError{Op: "upload dump", Err: err}
	}
	metrics.ObserveUploadDuration(target, d.Name, d.StorageType, time.Since(start))

	if err := verifyUploadSize(ctx, st, objectPath, localSize, p.logger); err != nil {
		return err
	}
	return nil
}

// verifyUploadSize checks that the uploaded object's size matches the local
// file. Catches silent truncation, network corruption, or partial writes.
func verifyUploadSize(ctx context.Context, st storage.Storage, objectPath string, expected int64, log logr.Logger) error {
	objs, err := st.List(ctx, objectPath)
	if err != nil {
		log.V(1).Info("post-upload verify: list failed, skipping", "path", objectPath, "err", err.Error())
		return nil
	}
	for _, o := range objs {
		if o.Path == objectPath || strings.HasSuffix(o.Path, "/"+path.Base(objectPath)) {
			if o.Size != expected {
				return &RetryableError{
					Op:  "upload verify",
					Err: fmt.Errorf("size mismatch for %s: local=%d remote=%d", objectPath, expected, o.Size),
				}
			}
			log.V(1).Info("post-upload verify passed", "path", objectPath, "size", expected)
			return nil
		}
	}
	log.V(1).Info("post-upload verify: object not found in listing, skipping", "path", objectPath)
	return nil
}

func (p *Pipeline) loadPreviousStats(ctx context.Context, dests []*secrets.Destination, target string) (*dumper.Stats, int64) {
	for _, d := range dests {
		st, err := storageFactory.NewStorage(d.StorageType, d.Name, d.Data, p.logger)
		if err != nil {
			continue
		}
		objs, err := st.List(ctx, target+"/")
		if err != nil || len(objs) == 0 {
			continue
		}
		// Walk metas newest-first and skip failure-metas: they carry no
		// stats and would otherwise blank the analyzer's comparison
		// baseline after a single failed run.
		for _, p := range sortedMetaPaths(objs) {
			rc, err := st.Get(ctx, p)
			if err != nil {
				continue
			}
			raw, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				continue
			}
			var m meta.MetaFile
			if err := json.Unmarshal(raw, &m); err != nil {
				continue
			}
			if m.IsFailure() {
				continue
			}
			return m.Stats, m.EncryptedSizeBytes
		}
	}
	return nil, 0
}

// sortedMetaPaths returns meta paths newest-first by LastModified.
func sortedMetaPaths(objs []storage.Object) []string {
	metas := make([]storage.Object, 0, len(objs))
	for _, o := range objs {
		if path.Ext(o.Path) != ".json" {
			continue
		}
		metas = append(metas, o)
	}
	sort.Slice(metas, func(i, j int) bool {
		return metas[i].LastModified.After(metas[j].LastModified)
	})
	out := make([]string, len(metas))
	for i, m := range metas {
		out[i] = m.Path
	}
	return out
}

func metaJSON(src *secrets.Source, stats *dumper.Stats, report *analyzer.Report, size int64, sha256sum, timestamp string, destResults []meta.DestinationResult) []byte {
	m := meta.MetaFile{
		Target:             src.TargetName,
		Timestamp:          timestamp,
		DBType:             src.DBType,
		Status:             meta.StatusSuccess,
		EncryptedSizeBytes: size,
		SHA256:             sha256sum,
		Stats:              stats,
		Report:             report,
		Destinations:       destResults,
	}
	out, _ := json.MarshalIndent(m, "", "  ")
	return out
}

// failureMetaJSON produces the sidecar written when a run never reaches the
// fan-out — there is no dump and no stats, only the cause and the phase
// where it broke.
func failureMetaJSON(src *secrets.Source, timestamp, phase string, runErr error) []byte {
	msg := ""
	if runErr != nil {
		msg = runErr.Error()
	}
	m := meta.MetaFile{
		Target:    src.TargetName,
		Timestamp: timestamp,
		DBType:    src.DBType,
		Status:    meta.StatusFailed,
		Error:     msg,
		Phase:     phase,
	}
	out, _ := json.MarshalIndent(m, "", "  ")
	return out
}

func emitAnalyzerMetrics(target string, r *analyzer.Report) {
	if r == nil {
		return
	}
	if r.SizeChangeRatio > 0 {
		metrics.SetDumpSizeChangeRatio(target, r.SizeChangeRatio)
	}
	metrics.SetSchemaChanged(target, r.SchemaChanged)
	if r.Current != nil {
		metrics.SetTableCount(target, len(r.Current.Tables))
		for _, t := range r.Current.Tables {
			metrics.SetTableRowCount(target, t.Name, t.RowCount)
		}
	}
	metrics.SetLastRunAnomalies(target, len(r.Anomalies))
}

func hashTableName(name string) string {
	h := sha256.Sum256([]byte(name))
	return hex.EncodeToString(h[:8])
}

func anonymizeStats(s *dumper.Stats) *dumper.Stats {
	anon := &dumper.Stats{
		SchemaHash:  s.SchemaHash,
		GeneratedAt: s.GeneratedAt,
		Tables:      make([]dumper.TableStats, len(s.Tables)),
	}
	for i, t := range s.Tables {
		anon.Tables[i] = dumper.TableStats{
			Name:      hashTableName(t.Name),
			RowCount:  t.RowCount,
			SizeBytes: t.SizeBytes,
		}
	}
	return anon
}

func anonymizeReport(r *analyzer.Report) *analyzer.Report {
	anon := &analyzer.Report{
		SizeChangeRatio: r.SizeChangeRatio,
		SchemaChanged:   r.SchemaChanged,
	}
	if r.Current != nil {
		anon.Current = anonymizeStats(r.Current)
	}
	if r.Previous != nil {
		anon.Previous = anonymizeStats(r.Previous)
	}
	if len(r.Anomalies) > 0 {
		anon.Anomalies = make([]analyzer.Anomaly, len(r.Anomalies))
		for i, a := range r.Anomalies {
			subj := a.Subject
			if subj != "" && subj != "<dump>" {
				subj = hashTableName(subj)
			}
			anon.Anomalies[i] = analyzer.Anomaly{
				Kind:    a.Kind,
				Subject: subj,
				Detail:  a.Detail,
			}
		}
	}
	if len(r.TableDiffs) > 0 {
		anon.TableDiffs = make([]analyzer.TableDiff, len(r.TableDiffs))
		for i, td := range r.TableDiffs {
			anon.TableDiffs[i] = analyzer.TableDiff{
				Name:           hashTableName(td.Name),
				PrevRows:       td.PrevRows,
				CurrRows:       td.CurrRows,
				RowChangeRatio: td.RowChangeRatio,
			}
		}
	}
	return anon
}

// recoverGoroutine catches panics in pipeline goroutines so a single
// destination failure doesn't crash the entire worker process.
func recoverGoroutine(log logr.Logger, phase, dest string) {
	if r := recover(); r != nil {
		log.Error(fmt.Errorf("panic: %v", r), "goroutine recovered",
			"phase", phase, "destination", dest)
	}
}

func buildObjectPath(target, timestamp, ext string) string {
	t, err := time.Parse("20060102T150405Z", timestamp)
	if err != nil {
		return path.Join(target, fmt.Sprintf("dump-%s.%s", timestamp, ext))
	}
	return path.Join(
		target,
		fmt.Sprintf("%04d", t.Year()),
		fmt.Sprintf("%02d", t.Month()),
		fmt.Sprintf("%02d", t.Day()),
		fmt.Sprintf("dump-%s.%s", timestamp, ext),
	)
}


