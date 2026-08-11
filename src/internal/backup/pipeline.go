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
//   1. CollectStats from the live DB (best-effort; missing stats just skip the analyzer step).
//   2. Dump → compress (gzip or zstd) → age → temp file (single dump regardless of N destinations).
//   3. Fan out the temp file to all destinations in parallel.
//   4. Write a sidecar meta JSON (unencrypted) with stats + analyzer report.
//   5. Compare with previous meta to populate analyzer metrics.
//   6. Apply retention policy per destination (best-effort, never fails the run).
type Pipeline struct {
	encryptor       crypto.Encryptor
	analyzer        analyzer.Analyzer
	tempDir         string
	logger          logr.Logger
	destProvider    DestinationProvider
	defaults        RetentionPolicy
	events          EventEmitter
	maxConcurrency  int
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
		p.recordFailure(ctx, dests, src, timestamp, "dumper-init", runStart, err, log)
		return fmt.Errorf("dumper: %w", err)
	}

	var preStats *dumper.Stats
	var preStatsError string
	if src.AnalyzerEnabled {
		s, statsErr := d.CollectStats(ctx)
		if statsErr != nil {
			// Sanitize before persisting/logging — driver errors can echo the
			// connection URI (and thus the password) back. Surfaced at Info
			// level and into meta.json so a silent permission failure stops
			// presenting as an unexplained "skipped" verifier verdict.
			preStatsError = dumper.SanitizeStderr(statsErr.Error(), src.Config.Password)
			log.Info("pre-dump stats collection failed; preStats unavailable for analyzer and verifier",
				"target", src.TargetName, "reason", preStatsError)
		} else {
			preStats = s
		}
	} else {
		log.V(1).Info("analyzer disabled by annotation; skipping stats collection")
	}

	if err := os.MkdirAll(p.tempDir, 0o755); err != nil {
		metrics.SetLastRunStatus(src.TargetName, false)
		p.events.Emit("Warning", "BackupFailed",
			fmt.Sprintf("Backup failed for target %s in phase temp-dir: %v", src.TargetName, err))
		p.recordFailure(ctx, dests, src, timestamp, "temp-dir", runStart, err, log)
		return fmt.Errorf("create temp dir: %w", err)
	}
	dumpFile := path.Join(p.tempDir, fmt.Sprintf("%s-%s.%s", src.TargetName, timestamp, labels.DumpSuffix(src.Compression)))

	// Decide whether THIS run gets a restore-verifier attached BEFORE we
	// build the encryptor — if yes, we generate an ephemeral keypair and
	// add its public half as an additional age recipient so the same
	// dump can be decrypted by either the long-lived DR key (offline)
	// or this single ephemeral identity (in-memory only). The ephemeral
	// identity is wiped at the end of Run via defer.
	//
	// ShouldVerify needs the latest meta to compute interval-elapsed,
	// so loading is pulled forward here. We re-use the same loader the
	// analyzer block uses below.
	var (
		ephID         *crypto.EphemeralIdentity
		runEncryptor  = p.encryptor
		preloadedMeta *meta.MetaFile
	)
	if p.verifierFactory != nil && src.RestoreVerificationMode != "" {
		preloadedMeta = p.loadPreviousMeta(ctx, dests, src.TargetName)
		shouldVerify, reason := verifier.ShouldVerify(src, preloadedMeta, runStart)
		log.V(1).Info("restore-verification decision", "should_verify", shouldVerify, "reason", reason, "mode", src.RestoreVerificationMode)
		if shouldVerify {
			id, err := crypto.GenerateEphemeralIdentity()
			if err != nil {
				log.Error(err, "ephemeral keypair generation failed; running without restore-verification this run")
			} else {
				re, err := crypto.NewEncryptorWithExtraRecipient(p.encryptor, id.PublicLine())
				if err != nil {
					log.Error(err, "encryptor extra-recipient wrap failed; running without restore-verification this run")
					id.Wipe()
				} else {
					ephID = id
					runEncryptor = re
					log.Info("restore-verification armed for this run", "mode", src.RestoreVerificationMode, "ephemeralFingerprint", id.RecipientFingerprint())
				}
			}
		}
	}
	defer func() {
		if ephID != nil {
			ephID.Wipe()
		}
	}()

	rowCounter := dumper.NewRowCounter(nil, src.DBType) // writer set inside dumpToFile
	dumpStart := time.Now()
	dumpRes, err := p.dumpToFileWithEncryptor(ctx, d, dumpFile, rowCounter, runEncryptor, src.Compression)
	encryptedSize, sha256sum := dumpRes.EncryptedSize, dumpRes.SHA256
	dumpDuration := time.Since(dumpStart)
	metrics.ObserveDumpDuration(src.TargetName, src.DBType, dumpDuration)

	if err != nil {
		metrics.SetLastRunStatus(src.TargetName, false)
		_ = os.Remove(dumpFile)
		p.events.Emit("Warning", "BackupFailed",
			fmt.Sprintf("Backup failed for target %s in phase dump: %v", src.TargetName, err))
		p.recordFailure(ctx, dests, src, timestamp, "dump", runStart, err, log)
		return fmt.Errorf("dump: %w", err)
	}
	defer func() { _ = os.Remove(dumpFile) }()

	if encryptedSize == 0 {
		metrics.SetLastRunStatus(src.TargetName, false)
		emptyErr := errors.New("dump produced zero bytes")
		p.events.Emit("Warning", "BackupFailed",
			fmt.Sprintf("Backup failed for target %s: dump produced zero bytes (possible empty database or dump tool misconfiguration)", src.TargetName))
		p.recordFailure(ctx, dests, src, timestamp, "dump-empty", runStart, emptyErr, log)
		return emptyErr
	}

	metrics.SetDumpSize(src.TargetName, encryptedSize)

	// Post-dump stats collection for verification.
	//
	// Only collected when the row counter is INACTIVE (mongo/redis). There the
	// pre/post comparison is the only verdict path BuildVerification has — no
	// dump-stream count exists. For SQL engines the row counter already saw
	// every INSERT/COPY row, so the verdict comes from dumpCounts-vs-preStats
	// and postStats would be purely informational meta content that nothing
	// downstream consumes for a decision. Skipping it there saves a full
	// CollectStats roundtrip (a pg_stat_user_tables / INFORMATION_SCHEMA query)
	// on every SQL backup — real load at 10k+ daily runs.
	var postStats *dumper.Stats
	if src.AnalyzerEnabled && !rowCounter.Active() {
		s, statsErr := d.CollectStats(ctx)
		if statsErr != nil {
			log.V(1).Info("post-dump stats collection skipped", "reason", statsErr.Error())
		} else {
			postStats = s
		}
	}

	// Build dump verification result.
	// When the row counter ran (SQL dumpers), pass its counts even if empty —
	// an empty map signals "the dump produced 0 INSERTs", which the empty-dump
	// detector needs to distinguish from "we cannot count rows on this format"
	// (mongo). For inactive counters (mongo, redis), pass nil so the
	// verification falls back to pre/post stats comparison.
	var dumpCounts map[string]int64
	if rowCounter.Active() {
		if scanErr := rowCounter.Err(); scanErr != nil {
			// The parser hit an error (typically a dump line exceeding the
			// scanner's 10 MB buffer). The counts are incomplete, so trusting
			// them would let the empty-dump detector hard-fail a perfectly good
			// large-row dump. Leave dumpCounts nil → BuildVerification falls
			// back to pre/post stats comparison instead of acting on bad data.
			log.Info("row counter parse error; skipping count-based empty-dump check, falling back to stats",
				"err", scanErr.Error())
		} else {
			dumpCounts = rowCounter.Counts()
		}
	}
	verification := meta.BuildVerification(preStats, postStats, dumpCounts, src.DBType, encryptedSize, dumpRes.RawSize)
	if verification.Verdict == meta.VerificationMismatch {
		log.Info("dump verification mismatch detected", "summary", verification.Summary)
	}

	// Hard-fail when the dump appears to be empty despite the source DB having
	// rows. Without this, mysqldump exits 0 on permission failures (it can list
	// the database but can't SELECT) and we'd happily store DDL-only artifacts.
	// Opt-out via backup.mogenius.io/empty-dump-check=false for schema-only
	// sources (e.g. an empty template DB).
	if src.EmptyDumpCheck && verification.LooksEmpty {
		metrics.SetLastRunStatus(src.TargetName, false)
		emptyErr := fmt.Errorf("empty dump detected: %s", verification.Summary)
		p.events.Emit("Warning", "BackupFailed",
			fmt.Sprintf("Backup failed for target %s: %s", src.TargetName, emptyErr.Error()))
		p.recordFailure(ctx, dests, src, timestamp, "dump-empty-content", runStart, emptyErr, log)
		return emptyErr
	}

	// Empty-dump detection needs a pre-dump stats baseline: SQL engines
	// compare stream row counts against preStats, mongo/redis compare the
	// encrypted size against preStats total. When stats are unavailable
	// (analyzer disabled, or CollectStats failed — e.g. redis AUTH on INFO),
	// BuildVerification returns "skipped" and the check silently can't fire,
	// so a header-only dump (server exited 0 but emitted nothing) would pass.
	// We do NOT hard-fail on an absolute byte floor: encryptedSize includes
	// the age header, whose size scales with recipient count, so any fixed
	// floor would either miss real failures or false-fail legitimate tiny
	// dumps — and a noisy false-positive is worse than a rare miss here.
	// Instead surface the degraded check loudly so the gap is visible.
	if src.EmptyDumpCheck && preStats == nil {
		log.Info("empty-dump check degraded: no pre-dump stats baseline; cannot verify the dump is non-empty",
			"target", src.TargetName, "dbType", src.DBType)
		p.events.Emit("Warning", "BackupEmptyCheckDegraded",
			fmt.Sprintf("Backup for target %s: empty-dump check could not run (no pre-dump stats baseline); confirm the dump is non-empty or set backup.mogenius.io/analyzer-enabled=true", src.TargetName))
	}

	if len(dests) == 0 {
		metrics.SetLastRunStatus(src.TargetName, false)
		return errors.New("no destinations configured")
	}

	if err := ctx.Err(); err != nil {
		metrics.SetLastRunStatus(src.TargetName, false)
		p.events.Emit("Warning", "BackupFailed",
			fmt.Sprintf("Backup cancelled for target %s after dump phase: %v", src.TargetName, err))
		p.recordFailure(ctx, dests, src, timestamp, "cancelled", runStart, err, log)
		return fmt.Errorf("cancelled after dump: %w", err)
	}

	objectPath := buildObjectPath(src.TargetName, timestamp, labels.DumpSuffix(src.Compression))
	metaPath := buildObjectPath(src.TargetName, timestamp, "meta.json")

	// Use preStats for analyzer comparison (same as before)
	stats := preStats
	var report *analyzer.Report
	var schemaChangedAt time.Time
	if src.AnalyzerEnabled {
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
		}
		// AnonymizeTables persists hashed table names into meta.json, so the
		// previous run's Stats.Tables[].Name is already in hashed form.
		// Compare against fresh real names would treat every prev-table as
		// "disappeared" — hash the current stats too so prev/curr names line
		// up and only real schema drift surfaces as anomalies. Real names
		// stay available via the original `stats` for the metrics path.
		//
		// Transition case: when anonymize-tables is freshly enabled, the
		// last meta.json was written with real names. Hashing only the
		// current stats then mismatches against still-real prev stats and
		// produces N false-positive `table-disappeared` anomalies for one
		// run. Detect that and hash prev too so the very first post-toggle
		// run is clean.
		cmpStats := stats
		cmpPrev := prevStats
		switch {
		case src.AnonymizeTables:
			if stats != nil {
				cmpStats = anonymizeStats(stats)
			}
			if prevStats != nil && !looksAnonymized(prevStats) {
				cmpPrev = anonymizeStats(prevStats)
			}
		case prevStats != nil && looksAnonymized(prevStats):
			// Reverse transition: anonymize-tables was just turned off. The
			// last meta.json holds hashed names; the current run produced
			// real names. Hashes are one-way, so the per-table comparison
			// can't be reconciled for this single run — clearing prev's
			// Tables suppresses N false-positive `table-disappeared`
			// anomalies. Schema-hash, charset and size comparisons remain
			// intact (they don't depend on table names). The next run
			// will have real names on both sides and analyzes normally.
			cleared := *prevStats
			cleared.Tables = nil
			cmpPrev = &cleared
			log.Info("anonymize-tables disabled; prev meta still hashed — skipping per-table comparison for this run only")
		}
		an := p.analyzerForSource(src)
		report = an.Compare(cmpPrev, cmpStats, prevSize, encryptedSize)
		emitAnalyzerMetrics(src.TargetName, report, stats)
		// Carry forward the schema-change timestamp: only bump it on real
		// drift, otherwise the caller can use the recorded value to age the
		// schema. First run with no prev: pin to current run timestamp.
		switch {
		case report != nil && report.SchemaChanged:
			schemaChangedAt = runStart.UTC()
		case !prevSchemaChangedAt.IsZero():
			schemaChangedAt = prevSchemaChangedAt
		default:
			schemaChangedAt = runStart.UTC()
		}
		metrics.SetSchemaLastChange(src.TargetName, schemaChangedAt)
	}

	metaStats := stats
	metaReport := report
	var metaVerification *meta.DumpVerification
	if src.AnonymizeTables {
		if stats != nil {
			metaStats = anonymizeStats(stats)
		}
		// `report` is already in hashed-name form because the analyzer was
		// fed cmpStats above (hashed when AnonymizeTables=true). Calling
		// anonymizeReport here would double-hash Anomalies subjects and
		// re-process Current/Previous tables that are already hashed —
		// metaReport keeps the report as-is.
		if verification != nil {
			metaVerification = anonymizeVerification(verification)
		}
	} else {
		metaVerification = verification
	}

	// Pre-upload retention sweep: free space on destinations BEFORE uploading.
	// Without this, a full storage stays full forever (upload fails → retention
	// never runs → deadlock). Safe because MinKeep protects the N most recent
	// existing backups. Best-effort: failures here do not abort the run.
	//
	// The pre-upload sweep's per-destination result is the one we persist
	// into meta.json. The post-upload sweep below runs after the meta is
	// already in storage, so its results would arrive too late for the same
	// artifact. Pre-upload is the load-bearing path anyway: if it fails,
	// storage is filling up; if post-upload fails, we just have one extra
	// timestamp until next run.
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
		metrics.SetLastRunStatus(src.TargetName, false)
		p.events.Emit("Warning", "BackupFailed",
			fmt.Sprintf("Backup failed for target %s: all %d destination uploads failed", src.TargetName, len(dests)))
		p.recordFailure(ctx, dests, src, timestamp, "upload", runStart, errors.New("all destination uploads failed"), log)
		return errors.New("all destination uploads failed")
	}

	// Phase 2 prologue: run the restore-verifier (if armed) against the
	// local temp file before we serialise the meta. We deliberately
	// verify the LOCAL file rather than re-downloading from a
	// destination — Storage Scrubber already covers byte-level upload
	// integrity (see CLAUDE.md §18 ADR), and the local file's
	// decryptability is the property we care about here.
	var restoreVerification *meta.RestoreVerificationResult
	if ephID != nil && p.verifierFactory != nil {
		restoreVerification = p.runRestoreVerifier(ctx, src, dumpFile, ephID, preStats, dumpCounts, log)
	} else if restoreVerificationActive(src) && preloadedMeta != nil {
		// This run did not verify (interval not yet elapsed). Carry the
		// previous run's verification result forward so its CompletedAt clock
		// persists in this meta.json. Without this, a skipped run writes a meta
		// with no RestoreVerification block; the next run reads that meta, sees
		// nil, takes ShouldVerify's "first verification" path, and verifies —
		// so the verifier fires every OTHER run instead of once per interval
		// (e.g. mode=full on an hourly schedule would spawn a full-restore pod
		// every 2h instead of weekly). Same carry-forward pattern as
		// schemaChangedAt. Gated on the mode being active so turning
		// verification off lets the block (and its refresher gauge) lapse.
		restoreVerification = preloadedMeta.RestoreVerification
	}

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
	log.Info("backup completed", "destinations_succeeded", successCount, "destinations_total", len(dests),
		"verification", verification.Verdict)

	// Post-upload retention: now that the fresh artifact is safely stored,
	// prune again in case the newly uploaded backup pushed the count above
	// the retention threshold. Best-effort: results are not persisted to
	// the (already-uploaded) meta.json, but per-destination outcomes are
	// logged and failures emit RetentionFailed Events so the audit trail
	// is complete. If a post-upload sweep persistently fails, storage
	// gradually grows by one extra cohort per run rather than catastrophically;
	// the pre-upload sweep is the load-bearing path with the alerting.
	if !policy.Disabled() {
		postResults := p.applyRetention(ctx, dests, src.TargetName, "post-upload", policy, time.Now(), log)
		for _, r := range postResults {
			if r.Status == meta.StatusFailed {
				log.Info("post-upload retention failed",
					"destination", r.Name, "target", src.TargetName, "err", r.Error)
				continue
			}
			if r.DeletedDumps+r.DeletedMetas+r.DeletedOther > 0 {
				log.Info("post-upload retention pruned",
					"destination", r.Name, "target", src.TargetName,
					"dumps", r.DeletedDumps, "metas", r.DeletedMetas, "other", r.DeletedOther)
			}
		}
	}

	return nil
}




