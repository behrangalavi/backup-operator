package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sync"
	"time"

	"backup-operator/analyzer"
	"backup-operator/crypto"
	"backup-operator/dumper"
	dumperFactory "backup-operator/dumper/factory"
	"backup-operator/internal/labels"
	"backup-operator/internal/meta"
	"backup-operator/internal/secrets"
	"backup-operator/metrics"
	"backup-operator/storage"
	storageFactory "backup-operator/storage/factory"
	"backup-operator/verifier"
	"backup-operator/verifier/ephemeral"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
//  1. CollectStats from the live DB (best-effort; missing stats just skip the analyzer step).
//  2. Dump → compress (gzip or zstd) → age → temp file (single dump regardless of N destinations).
//  3. Fan out the temp file to all destinations in parallel.
//  4. Write a sidecar meta JSON (unencrypted) with stats + analyzer report.
//  5. Compare with previous meta to populate analyzer metrics.
//  6. Apply retention policy per destination (best-effort, never fails the run).
type Pipeline struct {
	encryptor      crypto.Encryptor
	analyzer       analyzer.Analyzer
	tempDir        string
	logger         logr.Logger
	destProvider   DestinationProvider
	defaults       RetentionPolicy
	events         EventEmitter
	maxConcurrency int
	// verifierFactory builds a per-(mode, dbType) restore-verifier. nil
	// means restore-verification is disabled at this build of the
	// pipeline (used by tests and the legacy NewPipeline constructor).
	verifierFactory func(mode, dbType string, log logr.Logger) (verifier.Verifier, error)

	// Phase-2 fields, only used when a Phase-2 mode is selected on a
	// source. nil spawner means Phase-2 modes will fail with
	// "no spawner configured" — Phase-1 stream-validate is unaffected.
	spawner   ephemeral.Spawner
	namespace string
	ownerRef  *metav1.OwnerReference

	// storageCache reuses storage clients across upload, meta-upload,
	// and retention phases within a single Run(). The worker is one-shot
	// so this avoids 3×N TCP+TLS handshakes (N = destination count).
	// Guarded by storageMu for concurrent fan-out goroutines.
	storageMu    sync.Mutex
	storageCache map[string]storage.Storage
}

// DestinationProvider returns the current set of destinations at run time.
// Implemented by the controller cache so we always pick up new destinations.
type DestinationProvider interface {
	Destinations() []*secrets.Destination
}

// Option configures a Pipeline. Pass to NewPipeline.
type Option func(*Pipeline)

// WithEvents sets the EventEmitter for Kubernetes audit events.
func WithEvents(e EventEmitter) Option {
	return func(p *Pipeline) { p.events = e }
}

// WithVerifierFactory wires a restore-verifier factory. nil disables
// restore-verification.
func WithVerifierFactory(f func(mode, dbType string, log logr.Logger) (verifier.Verifier, error)) Option {
	return func(p *Pipeline) { p.verifierFactory = f }
}

// WithRestoreSpawner wires the ephemeral-DB spawning context the
// Phase-2 restore verifier needs. Pass spawner=nil if you want
// Phase-1 stream-validate only — Phase-2 verifiers will then refuse
// to run with a clear error rather than half-execute.
func WithRestoreSpawner(s ephemeral.Spawner, namespace string, owner *metav1.OwnerReference) Option {
	return func(p *Pipeline) {
		p.spawner = s
		p.namespace = namespace
		p.ownerRef = owner
	}
}

// NewPipeline creates a Pipeline with the required dependencies and any
// optional settings. Use WithEvents, WithVerifierFactory, and
// WithRestoreSpawner to configure optional behavior.
func NewPipeline(
	enc crypto.Encryptor,
	an analyzer.Analyzer,
	tempDir string,
	dp DestinationProvider,
	defaults RetentionPolicy,
	logger logr.Logger,
	opts ...Option,
) *Pipeline {
	p := &Pipeline{
		encryptor:      enc,
		analyzer:       an,
		tempDir:        tempDir,
		logger:         logger,
		destProvider:   dp,
		defaults:       defaults,
		events:         NoopEventEmitter{},
		maxConcurrency: defaultMaxConcurrency,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// getStorage returns a cached storage client for the destination, creating
// one on first access. Thread-safe for concurrent fan-out goroutines.
func (p *Pipeline) getStorage(d *secrets.Destination) (storage.Storage, error) {
	p.storageMu.Lock()
	defer p.storageMu.Unlock()
	if p.storageCache == nil {
		p.storageCache = make(map[string]storage.Storage)
	}
	if st, ok := p.storageCache[d.Name]; ok {
		return st, nil
	}
	st, err := storageFactory.NewStorage(d.StorageType, d.Name, d.Data, p.logger)
	if err != nil {
		return nil, err
	}
	p.storageCache[d.Name] = st
	return st, nil
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

// restoreVerificationActive reports whether restore-verification is switched on
// for a source (any mode other than off/empty). Used to decide whether to carry
// a previous run's verification result forward into a run that didn't verify.
func restoreVerificationActive(src *secrets.Source) bool {
	m := src.RestoreVerificationMode
	return m != "" && m != labels.RestoreVerificationOff
}

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
	rc := runContext{ctx: ctx, src: src, dests: dests, timestamp: timestamp, runStart: runStart, log: log}

	d, err := dumperFactory.NewDumper(src.DBType, src.Config, log)
	if err != nil {
		return p.failRun(rc, "dumper-init", err, fmt.Errorf("dumper: %w", err),
			fmt.Sprintf("Backup failed for target %s in phase dumper-init: %v", src.TargetName, err))
	}

	preStats, preStatsError := p.collectPreStats(ctx, d, src, log)

	if err := os.MkdirAll(p.tempDir, 0o755); err != nil {
		return p.failRun(rc, "temp-dir", err, fmt.Errorf("create temp dir: %w", err),
			fmt.Sprintf("Backup failed for target %s in phase temp-dir: %v", src.TargetName, err))
	}
	dumpFile := path.Join(p.tempDir, fmt.Sprintf("%s-%s.%s", src.TargetName, timestamp, labels.DumpSuffix(src.Compression)))

	// Decide whether THIS run gets a restore-verifier attached BEFORE we
	// build the encryptor — if yes, an ephemeral keypair's public half is
	// added as an additional age recipient so the same dump can be decrypted
	// by either the long-lived DR key (offline) or this single in-memory
	// identity, which is wiped at the end of Run.
	arm := p.armRestoreVerifier(ctx, src, dests, runStart, log)
	defer arm.wipe()

	rowCounter := dumper.NewRowCounter(nil, src.DBType) // writer set inside dumpToFile
	dumpStart := time.Now()
	dumpRes, err := p.dumpToFileWithEncryptor(ctx, d, dumpFile, rowCounter, arm.encryptor, src.Compression)
	encryptedSize, sha256sum := dumpRes.EncryptedSize, dumpRes.SHA256
	dumpDuration := time.Since(dumpStart)
	metrics.ObserveDumpDuration(src.TargetName, src.DBType, dumpDuration)

	if err != nil {
		_ = os.Remove(dumpFile)
		return p.failRun(rc, "dump", err, fmt.Errorf("dump: %w", err),
			fmt.Sprintf("Backup failed for target %s in phase dump: %v", src.TargetName, err))
	}
	defer func() { _ = os.Remove(dumpFile) }()

	if encryptedSize == 0 {
		emptyErr := errors.New("dump produced zero bytes")
		return p.failRun(rc, "dump-empty", emptyErr, emptyErr,
			fmt.Sprintf("Backup failed for target %s: dump produced zero bytes (possible empty database or dump tool misconfiguration)", src.TargetName))
	}

	metrics.SetDumpSize(src.TargetName, encryptedSize)

	postStats := p.collectPostStats(ctx, d, src, rowCounter, log)
	dumpCounts := resolveDumpCounts(rowCounter, log)
	verification := meta.BuildVerification(preStats, postStats, dumpCounts, src.DBType, encryptedSize, dumpRes.RawSize)
	if verification.Verdict == meta.VerificationMismatch {
		log.Info("dump verification mismatch detected", "summary", verification.Summary)
	}

	// Hard-fail when the dump appears to be empty despite the source DB having
	// rows. Without this, mysqldump exits 0 on permission failures (it can list
	// the database but can't SELECT) and we'd happily store DDL-only artifacts.
	// Opt-out via spec.analyzer.emptyDumpCheck=false for schema-only sources.
	if src.EmptyDumpCheck && verification.LooksEmpty {
		emptyErr := fmt.Errorf("empty dump detected: %s", verification.Summary)
		return p.failRun(rc, "dump-empty-content", emptyErr, emptyErr,
			fmt.Sprintf("Backup failed for target %s: %s", src.TargetName, emptyErr.Error()))
	}
	p.warnEmptyCheckDegraded(src, preStats, log)

	if len(dests) == 0 {
		metrics.SetLastRunStatus(src.TargetName, false)
		return errors.New("no destinations configured")
	}

	if err := ctx.Err(); err != nil {
		return p.failRun(rc, "cancelled", err, fmt.Errorf("cancelled after dump: %w", err),
			fmt.Sprintf("Backup cancelled for target %s after dump phase: %v", src.TargetName, err))
	}

	objectPath := buildObjectPath(src.TargetName, timestamp, labels.DumpSuffix(src.Compression))
	metaPath := buildObjectPath(src.TargetName, timestamp, "meta.json")

	stats := preStats
	report, schemaChangedAt := p.analyzeRun(ctx, src, dests, arm.preloadedMeta, stats, encryptedSize, runStart, log)
	metaStats, metaReport, metaVerification := metaViewOf(src, stats, report, verification)

	// Pre-upload retention sweep: free space on destinations BEFORE uploading.
	// Without this, a full storage stays full forever (upload fails → retention
	// never runs → deadlock). Safe because MinKeep protects the N most recent
	// existing backups. Best-effort: failures here do not abort the run. Its
	// per-destination result is the one persisted into meta.json — the
	// post-upload sweep runs after the meta is already in storage.
	policy := p.resolvePolicy(src)
	var retentionResults []meta.RetentionResult
	if !policy.Disabled() {
		log.V(1).Info("running pre-upload retention sweep")
		retentionResults = p.applyRetention(ctx, dests, src.TargetName, "pre-upload", policy, time.Now(), log)
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
		uploadErr := errors.New("all destination uploads failed")
		return p.failRun(rc, "upload", uploadErr, uploadErr,
			fmt.Sprintf("Backup failed for target %s: all %d destination uploads failed", src.TargetName, len(dests)))
	}

	restoreVerification := p.resolveRestoreVerification(ctx, src, dumpFile, arm, preStats, dumpCounts, log)

	// Phase 2: build meta with destination results, upload to successful destinations.
	metaBytes, marshalErr := metaJSON(src, metaStats, preStatsError, metaReport, metaVerification, encryptedSize, sha256sum, timestamp, runStart, dumpDuration, schemaChangedAt, destResults, restoreVerification, retentionResults)
	if marshalErr != nil {
		// Marshal of basic structs is essentially never expected to fail.
		// If it does (future schema change adds a non-marshalable field),
		// upload a minimal hand-built meta so the run still surfaces in
		// the UI and the failure becomes visible rather than silent.
		log.Error(marshalErr, "meta marshal failed; uploading fallback meta", "target", src.TargetName)
		metaBytes = fallbackMetaJSON(src.TargetName, timestamp, src.DBType, "meta-marshal", marshalErr)
	}
	metaAttempted, metaSucceeded := p.uploadMeta(ctx, dests, destResults, metaPath, metaBytes, log)
	if metaAttempted > 0 && metaSucceeded == 0 {
		// Every destination that received the dump failed to receive its
		// meta.json sidecar. The dumps physically exist but are invisible to
		// the metrics refresher, the UI, and the restore listing (all key off
		// meta.json), and un-analyzable (no baseline for the next run). Report
		// this as a failure so the Job is marked failed and K8s retries with a
		// fresh timestamp, rather than silently claiming success — a phantom
		// backup. The orphan dumps are pruned by retention on a later run.
		metrics.SetLastRunStatus(src.TargetName, false)
		p.events.Emit("Warning", "BackupFailed",
			fmt.Sprintf("Backup for target %s uploaded the dump but meta.json failed on all %d destination(s); run is not discoverable", src.TargetName, metaAttempted))
		log.Error(errors.New("meta upload failed on all destinations"), "phantom backup avoided: failing run so it retries",
			"target", src.TargetName, "destinations", metaAttempted)
		return fmt.Errorf("meta.json upload failed on all %d destination(s)", metaAttempted)
	}
	if metaSucceeded < metaAttempted {
		// Partial: at least one destination has the sidecar, so the run is
		// still discoverable. Surface the gap as a warning Event so the
		// per-destination divergence is auditable, but don't fail the run.
		p.events.Emit("Warning", "BackupMetaPartial",
			fmt.Sprintf("Backup for target %s: meta.json reached %d of %d destination(s)", src.TargetName, metaSucceeded, metaAttempted))
	}

	metrics.SetLastRunStatus(src.TargetName, true)
	p.events.Emit("Normal", "BackupCompleted",
		fmt.Sprintf("Backup completed for target %s (%d/%d destinations, %d bytes, verification: %s)",
			src.TargetName, successCount, len(dests), encryptedSize, verification.Verdict))
	log.Info("backup completed", "destinationsSucceeded", successCount, "destinationsTotal", len(dests),
		"verification", verification.Verdict)

	if !policy.Disabled() {
		p.postUploadRetention(ctx, dests, src, policy, log)
	}
	return nil
}

// runContext bundles the identifiers every failure path needs.
type runContext struct {
	ctx       context.Context
	src       *secrets.Source
	dests     []*secrets.Destination
	timestamp string
	runStart  time.Time
	log       logr.Logger
}

// failRun is the single failure path: mark the run failed, emit the
// BackupFailed Event, persist a failure meta (so the run is discoverable) and
// hand back the error the caller returns. recordErr is what lands in the
// failure meta, returnErr the (usually wrapped) error for the worker's exit.
func (p *Pipeline) failRun(rc runContext, phase string, recordErr, returnErr error, eventMsg string) error {
	metrics.SetLastRunStatus(rc.src.TargetName, false)
	p.events.Emit("Warning", "BackupFailed", eventMsg)
	p.recordFailure(rc.ctx, rc.dests, rc.src, rc.timestamp, phase, rc.runStart, recordErr, rc.log)
	return returnErr
}

// collectPreStats runs the pre-dump stats query when the analyzer is on.
// Failure is non-fatal: the sanitized reason is persisted into meta.json so a
// silent permission failure stops presenting as an unexplained "skipped"
// verifier verdict.
func (p *Pipeline) collectPreStats(ctx context.Context, d dumper.Dumper, src *secrets.Source, log logr.Logger) (*dumper.Stats, string) {
	if !src.AnalyzerEnabled {
		log.V(1).Info("analyzer disabled by spec; skipping stats collection")
		return nil, ""
	}
	s, statsErr := d.CollectStats(ctx)
	if statsErr != nil {
		// Sanitize before persisting/logging — driver errors can echo the
		// connection URI (and thus the password) back.
		reason := dumper.SanitizeStderr(statsErr.Error(), src.Config.Password)
		log.Info("pre-dump stats collection failed; preStats unavailable for analyzer and verifier",
			"target", src.TargetName, "reason", reason)
		return nil, reason
	}
	return s, ""
}

// collectPostStats re-queries stats after the dump ONLY for engines whose
// row counter is inactive (mongo/redis): there the pre/post comparison is the
// only verdict path BuildVerification has. For SQL engines the row counter
// already saw every INSERT/COPY row and postStats would be purely
// informational, so the second CollectStats roundtrip is skipped — real load
// at 10k+ daily runs.
func (p *Pipeline) collectPostStats(ctx context.Context, d dumper.Dumper, src *secrets.Source, rowCounter *dumper.RowCounter, log logr.Logger) *dumper.Stats {
	if !src.AnalyzerEnabled || rowCounter.Active() {
		return nil
	}
	s, statsErr := d.CollectStats(ctx)
	if statsErr != nil {
		log.V(1).Info("post-dump stats collection skipped", "reason", statsErr.Error())
		return nil
	}
	return s
}

// resolveDumpCounts returns the row counter's per-table counts for SQL
// engines (an empty map means "the dump produced 0 INSERTs", which the
// empty-dump detector needs), nil for inactive counters (mongo/redis → fall
// back to pre/post stats) and nil when the scan errored — a dump line past
// the scanner buffer leaves the counts incomplete, and trusting them would
// let the empty-dump detector hard-fail a perfectly good large-row dump.
func resolveDumpCounts(rowCounter *dumper.RowCounter, log logr.Logger) map[string]int64 {
	if !rowCounter.Active() {
		return nil
	}
	if scanErr := rowCounter.Err(); scanErr != nil {
		log.Info("row counter parse error; skipping count-based empty-dump check, falling back to stats",
			"err", scanErr.Error())
		return nil
	}
	return rowCounter.Counts()
}

// warnEmptyCheckDegraded surfaces that the empty-dump check could not run:
// it needs a pre-dump stats baseline (SQL compares stream row counts against
// it, mongo/redis compare the dump size against its total). We deliberately
// do NOT hard-fail on an absolute byte floor — encryptedSize includes the age
// header, whose size scales with recipient count, so any fixed floor would
// either miss real failures or false-fail legitimate tiny dumps.
func (p *Pipeline) warnEmptyCheckDegraded(src *secrets.Source, preStats *dumper.Stats, log logr.Logger) {
	if !src.EmptyDumpCheck || preStats != nil {
		return
	}
	log.Info("empty-dump check degraded: no pre-dump stats baseline; cannot verify the dump is non-empty",
		"target", src.TargetName, "dbType", src.DBType)
	p.events.Emit("Warning", "BackupEmptyCheckDegraded",
		fmt.Sprintf("Backup for target %s: empty-dump check could not run (no pre-dump stats baseline); confirm the dump is non-empty or set spec.analyzer.enabled=true", src.TargetName))
}

// armedVerifier is the outcome of the pre-dump restore-verification decision.
type armedVerifier struct {
	id            *crypto.EphemeralIdentity
	encryptor     crypto.Encryptor
	preloadedMeta *meta.MetaFile
}

func (a *armedVerifier) wipe() {
	if a.id != nil {
		a.id.Wipe()
	}
}

// armRestoreVerifier decides whether this run verifies and, if so, wraps the
// encryptor with an ephemeral recipient. ShouldVerify needs the latest meta
// to compute interval-elapsed, so the baseline load is pulled forward here
// and reused by the analyzer. Any failure to arm degrades to "no verification
// this run" — never to a failed backup.
func (p *Pipeline) armRestoreVerifier(ctx context.Context, src *secrets.Source, dests []*secrets.Destination, runStart time.Time, log logr.Logger) armedVerifier {
	arm := armedVerifier{encryptor: p.encryptor}
	if p.verifierFactory == nil || src.RestoreVerificationMode == "" {
		return arm
	}
	arm.preloadedMeta = p.loadPreviousMeta(ctx, dests, src.TargetName)
	shouldVerify, reason := verifier.ShouldVerify(src, arm.preloadedMeta, runStart)
	log.V(1).Info("restore-verification decision", "shouldVerify", shouldVerify, "reason", reason, "mode", src.RestoreVerificationMode)
	if !shouldVerify {
		return arm
	}
	id, err := crypto.GenerateEphemeralIdentity()
	if err != nil {
		log.Error(err, "ephemeral keypair generation failed; running without restore-verification this run")
		return arm
	}
	re, err := crypto.NewEncryptorWithExtraRecipient(p.encryptor, id.PublicLine())
	if err != nil {
		log.Error(err, "encryptor extra-recipient wrap failed; running without restore-verification this run")
		id.Wipe()
		return arm
	}
	arm.id, arm.encryptor = id, re
	log.Info("restore-verification armed for this run", "mode", src.RestoreVerificationMode, "ephemeralFingerprint", id.RecipientFingerprint())
	return arm
}

// resolveRestoreVerification runs the armed verifier against the LOCAL temp
// file (the Storage Scrubber already covers byte-level upload integrity) or,
// when this run was not due, carries the previous verdict forward so its
// CompletedAt clock persists — otherwise the next run would see no block,
// take ShouldVerify's "first verification" path, and the verifier would fire
// every other run instead of once per interval. A budget timeout is handled
// inside runRestoreVerifier (it re-stamps the previous verdict).
func (p *Pipeline) resolveRestoreVerification(ctx context.Context, src *secrets.Source, dumpFile string, arm armedVerifier, preStats *dumper.Stats, dumpCounts map[string]int64, log logr.Logger) *meta.RestoreVerificationResult {
	var prevRV *meta.RestoreVerificationResult
	if arm.preloadedMeta != nil {
		prevRV = arm.preloadedMeta.RestoreVerification
	}
	if arm.id != nil && p.verifierFactory != nil {
		if rv := p.runRestoreVerifier(ctx, src, dumpFile, arm.id, preStats, dumpCounts, prevRV, log); rv != nil {
			return rv
		}
	}
	if restoreVerificationActive(src) && arm.preloadedMeta != nil {
		return arm.preloadedMeta.RestoreVerification
	}
	return nil
}

// analyzeRun compares this run's stats against the previous meta and derives
// the carried-forward schema-change timestamp. Returns (nil, zero) when the
// analyzer is disabled.
func (p *Pipeline) analyzeRun(ctx context.Context, src *secrets.Source, dests []*secrets.Destination, preloadedMeta *meta.MetaFile, stats *dumper.Stats, encryptedSize int64, runStart time.Time, log logr.Logger) (*analyzer.Report, time.Time) {
	if !src.AnalyzerEnabled {
		return nil, time.Time{}
	}
	prevMeta := preloadedMeta
	if prevMeta == nil {
		prevMeta = p.loadPreviousMeta(ctx, dests, src.TargetName)
	}
	var prevStats *dumper.Stats
	var prevSize int64
	var prevSchemaChangedAt time.Time
	if prevMeta != nil {
		prevStats = prevMeta.Stats
		prevSize = prevMeta.EncryptedSizeBytes
		prevSchemaChangedAt = prevMeta.SchemaChangedAt
		// SizeChangeRatio compares ENCRYPTED+COMPRESSED sizes across runs. If
		// the compression algorithm changed since the previous run (gzip→zstd
		// is 30-50% smaller) the ratio would trip BackupDumpSizeCollapsed on
		// a healthy dump — zero prevSize for this one run so the size check
		// is skipped; the next run compares like-for-like. Empty
		// prevMeta.Compression means a legacy gzip meta.
		prevComp := prevMeta.Compression
		if prevComp == "" {
			prevComp = labels.CompressionGzip
		}
		if prevComp != src.Compression {
			log.Info("compression changed since last run; skipping size-collapse check for this run",
				"previous", prevComp, "current", src.Compression)
			prevSize = 0
		}
	}
	cmpStats, cmpPrev := comparableStats(src, stats, prevStats, log)
	report := p.analyzerForSource(src).Compare(cmpPrev, cmpStats, prevSize, encryptedSize)
	emitAnalyzerMetrics(src.TargetName, report, stats)
	// Carry forward the schema-change timestamp: only bump it on real drift,
	// otherwise the caller can use the recorded value to age the schema.
	// First run with no prev: pin to current run timestamp.
	var schemaChangedAt time.Time
	switch {
	case report != nil && report.SchemaChanged:
		schemaChangedAt = runStart.UTC()
	case !prevSchemaChangedAt.IsZero():
		schemaChangedAt = prevSchemaChangedAt
	default:
		schemaChangedAt = runStart.UTC()
	}
	metrics.SetSchemaLastChange(src.TargetName, schemaChangedAt)
	return report, schemaChangedAt
}

// comparableStats lines prev and current stats up for the analyzer when
// table-name anonymisation is involved. AnonymizeTables persists hashed names
// into meta.json, so comparing fresh real names against a hashed prev would
// treat every prev table as "disappeared". Both toggle transitions are
// handled: freshly enabled → hash prev too; freshly disabled → prev is hashed
// one-way and can't be reconciled, so its tables are cleared for this single
// run (schema-hash, charset and size comparisons remain intact).
func comparableStats(src *secrets.Source, stats, prevStats *dumper.Stats, log logr.Logger) (cmpStats, cmpPrev *dumper.Stats) {
	cmpStats, cmpPrev = stats, prevStats
	switch {
	case src.AnonymizeTables:
		if stats != nil {
			cmpStats = anonymizeStats(stats)
		}
		if prevStats != nil && !looksAnonymized(prevStats) {
			cmpPrev = anonymizeStats(prevStats)
		}
	case prevStats != nil && looksAnonymized(prevStats):
		cleared := *prevStats
		cleared.Tables = nil
		cmpPrev = &cleared
		log.Info("anonymize-tables disabled; prev meta still hashed — skipping per-table comparison for this run only")
	}
	return cmpStats, cmpPrev
}

// metaViewOf applies table-name anonymisation to what lands in meta.json.
// `report` is already in hashed-name form because the analyzer was fed the
// hashed stats, so it is kept as-is (anonymising it again would double-hash).
func metaViewOf(src *secrets.Source, stats *dumper.Stats, report *analyzer.Report, verification *meta.DumpVerification) (*dumper.Stats, *analyzer.Report, *meta.DumpVerification) {
	if !src.AnonymizeTables {
		return stats, report, verification
	}
	metaStats := stats
	if stats != nil {
		metaStats = anonymizeStats(stats)
	}
	var metaVerification *meta.DumpVerification
	if verification != nil {
		metaVerification = anonymizeVerification(verification)
	}
	return metaStats, report, metaVerification
}

// postUploadRetention prunes again now that the fresh artifact is stored, in
// case it pushed the count above the threshold. Best-effort and not persisted
// to the (already-uploaded) meta.json; outcomes are logged and failures emit
// RetentionFailed Events. The pre-upload sweep is the load-bearing path.
func (p *Pipeline) postUploadRetention(ctx context.Context, dests []*secrets.Destination, src *secrets.Source, policy RetentionPolicy, log logr.Logger) {
	for _, r := range p.applyRetention(ctx, dests, src.TargetName, "post-upload", policy, time.Now(), log) {
		if r.Status == meta.StatusFailed {
			log.Info("post-upload retention failed", "destination", r.Name, "target", src.TargetName, "err", r.Error)
			continue
		}
		if r.DeletedDumps+r.DeletedMetas+r.DeletedOther > 0 {
			log.Info("post-upload retention pruned", "destination", r.Name, "target", src.TargetName,
				"dumps", r.DeletedDumps, "metas", r.DeletedMetas, "other", r.DeletedOther)
		}
	}
}
