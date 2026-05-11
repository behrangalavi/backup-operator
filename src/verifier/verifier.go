// Package verifier implements the restore-verification phase of a backup
// run. It proves the encrypted artifact just uploaded is decryptable and
// parseable, by streaming it through a per-mode parser using an ephemeral
// age identity that lives only inside the running worker pod (see ADR in
// CLAUDE.md §18).
//
// Two phases ship today:
//   - Phase 1 (ModeStreamValidate): in-process header + row-count parsing,
//     no DB-pod-spawn, no extra RBAC.
//   - Phase 2 (ModeSchemaOnly / ModeSample / ModeFull): spawn an ephemeral
//     DB pod via verifier/ephemeral, restore the dump, run smoke queries.
//     Gated behind restoreVerification.enableEphemeralPodSpawn=true in the
//     chart so the worker SA gets pods CRUD in its own namespace.
package verifier

import (
	"context"
	"time"

	"backup-operator/crypto"
	"backup-operator/dumper"
	"backup-operator/internal/labels"
	"backup-operator/internal/meta"
	"backup-operator/internal/secrets"
	"backup-operator/verifier/ephemeral"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Verifier produces a RestoreVerificationResult for one backup run.
// Implementations are picked from a factory keyed by (mode, dbType).
type Verifier interface {
	// Mode returns one of the labels.RestoreVerification* mode constants.
	Mode() string

	// Verify reads the encrypted dump file at dumpPath using the given
	// ephemeral identity, parses according to the verifier's mode, and
	// returns a result. The identity is valid only for the lifetime of
	// the call — callers must Wipe() it afterwards.
	//
	// Error from Verify means "the verifier could not run" (not "the
	// dump is bad"); a bad dump produces a Result with Verdict =
	// VerificationMismatch and a Summary explaining why.
	Verify(ctx context.Context, in Input) (*meta.RestoreVerificationResult, error)
}

// Input bundles every piece of context the verifier needs. Kept as a
// struct so adding inputs (e.g. preStats subset for sample mode) does
// not break implementations.
type Input struct {
	Source      *secrets.Source
	DumpPath    string
	Identity    *crypto.EphemeralIdentity
	PreStats    *dumper.Stats
	DumpRows    map[string]int64 // optional; nil for non-SQL paths
	Compression string           // "gzip" (default/empty) or "zstd"
	StartedAt   time.Time
	Logger      logr.Logger

	// Phase-2 fields. nil for stream-validate / off — the stream
	// verifier ignores them, the restore verifier requires them.
	Spawner   ephemeral.Spawner
	Namespace string
	OwnerRef  *metav1.OwnerReference
}

// Factory returns the Verifier matching the source's mode + dbType. Phase
// 1 only knows ModeStreamValidate; unknown modes return (nil, nil) so
// callers can treat that as "no-op for this run".
type Factory func(mode, dbType string, log logr.Logger) (Verifier, error)

// ShouldVerify decides whether the current run should generate an
// ephemeral keypair and run the verifier. State-driven: looks at the
// CompletedAt of the last RestoreVerification across all known metas
// to decide whether the configured interval has elapsed.
//
// Returns false (and a short reason) for any of:
//   - mode is "off" or empty
//   - latestMeta is nil (first run for this target — verifier needs a
//     baseline that DumpVerification has already covered)
//   - latestMeta has no RestoreVerification yet — fall through to the
//     "first verification" path: run it now (so the user sees signal
//     immediately rather than waiting one full interval after enabling)
//   - now - latestMeta.RestoreVerification.CompletedAt < interval
//
// The reason is logged at V(1) so operators can see why a run skipped
// verification without needing trace-level logging.
func ShouldVerify(src *secrets.Source, latestMeta *meta.MetaFile, now time.Time) (bool, string) {
	if src == nil {
		return false, "nil source"
	}
	mode := src.RestoreVerificationMode
	if mode == "" || mode == labels.RestoreVerificationOff {
		return false, "mode=off"
	}
	interval := src.RestoreVerificationInterval
	if interval <= 0 {
		interval = secrets.DefaultRestoreVerificationInterval
	}
	if latestMeta == nil {
		// No prior run at all — let the dump phase establish a baseline
		// first. Verification next time. This prevents a brand-new
		// source from running a verifier with no preStats to compare.
		return false, "no prior run"
	}
	if latestMeta.RestoreVerification == nil || latestMeta.RestoreVerification.CompletedAt.IsZero() {
		// Configured but never verified — run it now.
		return true, "first verification"
	}
	since := now.Sub(latestMeta.RestoreVerification.CompletedAt)
	if since < interval {
		return false, "interval not elapsed"
	}
	return true, "interval elapsed"
}

// FailureResult builds a RestoreVerificationResult for the case where the
// verifier itself blew up (couldn't read the file, decrypt failed, parser
// crashed). Distinct from a "ran successfully and found a mismatch" result.
func FailureResult(mode string, started time.Time, err error, fingerprint string) *meta.RestoreVerificationResult {
	now := time.Now().UTC()
	return &meta.RestoreVerificationResult{
		Mode:                          mode,
		Verdict:                       meta.VerificationSkipped,
		Summary:                       "verifier failed: " + safeErr(err),
		Error:                         safeErr(err),
		StartedAt:                     started,
		CompletedAt:                   now,
		DurationSeconds:               now.Sub(started).Seconds(),
		EphemeralRecipientFingerprint: fingerprint,
	}
}

func safeErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
