package restore

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"backup-operator/crypto"
	"backup-operator/dumper"
	"backup-operator/internal/meta"
	"backup-operator/internal/secrets"
	"backup-operator/verifier"
)

// New returns a Verifier that performs an actual restore against a
// short-lived DB pod spawned via Input.Spawner. mode picks the
// per-engine restore depth.
func New(mode Mode, dbType string, log logr.Logger) (verifier.Verifier, error) {
	switch mode {
	case ModeSchemaOnly, ModeSample, ModeFull:
	default:
		return nil, fmt.Errorf("restore verifier: unknown mode %q", mode)
	}
	engine, err := NewEngine(dbType)
	if err != nil {
		return nil, err
	}
	return &restoreVerifier{
		mode:   mode,
		engine: engine,
		log:    log.WithName(string(mode)),
	}, nil
}

type restoreVerifier struct {
	mode   Mode
	engine Engine
	log    logr.Logger
}

func (v *restoreVerifier) Mode() string { return string(v.mode) }

// Verify runs the full sequence: spawn → wait → restore → smoke → stop.
// Spawner failures (RBAC denied, image pull, schedule timeout) surface
// as Verdict=Skipped with the underlying error in the result;
// content-mismatch (smoke queries failed) is Verdict=Mismatch. The
// pod stop is best-effort; OwnerReference cascade handles cleanup if
// Stop fails.
func (v *restoreVerifier) Verify(ctx context.Context, in verifier.Input) (*meta.RestoreVerificationResult, error) {
	started := in.StartedAt
	if started.IsZero() {
		started = time.Now().UTC()
	}
	fingerprint := ""
	if in.Identity != nil {
		fingerprint = in.Identity.RecipientFingerprint()
	}

	if in.Spawner == nil {
		return verifier.FailureResult(v.Mode(), started, errors.New("no spawner configured for restore verifier"), fingerprint), nil
	}
	if in.OwnerRef == nil {
		v.log.V(1).Info("no OwnerRef in Input — spawned pods rely on Stop() best-effort cleanup")
	}

	volumeBytes := resolveVolumeBytes(in.Source, v.mode)
	imageOverride := imageOverrideFromSource(in.Source)

	spec := v.engine.PodSpec(volumeBytes, imageOverride)
	if in.Source != nil {
		spec.NamePrefix = fmt.Sprintf("verify-%s", in.Source.TargetName)
	}
	spec.OwnerRef = in.OwnerRef

	v.log.Info("spawning verifier DB pod",
		"mode", v.mode, "image", spec.Image, "volumeBytes", volumeBytes)
	db, err := in.Spawner.Spawn(ctx, spec, v.log)
	if err != nil {
		return verifier.FailureResult(v.Mode(), started, fmt.Errorf("spawn ephemeral DB: %w", err), fingerprint), nil
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = db.Stop(stopCtx)
	}()

	if err := db.Wait(ctx); err != nil {
		return v.failureWith(started, fingerprint, fmt.Errorf("ephemeral DB never became ready: %w", err)), nil
	}

	plaintext, closer, err := openDecryptedStream(in.DumpPath, in.Identity)
	if err != nil {
		return v.failureWith(started, fingerprint, fmt.Errorf("open decrypted stream: %w", err)), nil
	}
	defer closer()

	if err := v.engine.Restore(ctx, db.Endpoint(), plaintext, v.mode, v.log); err != nil {
		return v.mismatchWith(started, fingerprint, fmt.Sprintf("restore failed: %v", err)), nil
	}

	smoke, err := v.engine.SmokeQueries(ctx, db.Endpoint(), totalsByTable(in.PreStats), v.mode, v.log)
	if err != nil {
		return v.failureWith(started, fingerprint, fmt.Errorf("smoke queries: %w", err)), nil
	}

	verdict, summary := evaluateSmoke(v.mode, smoke)
	completed := time.Now().UTC()
	return &meta.RestoreVerificationResult{
		Mode:                          v.Mode(),
		Verdict:                       verdict,
		Summary:                       summary,
		StartedAt:                     started,
		CompletedAt:                   completed,
		DurationSeconds:               completed.Sub(started).Seconds(),
		EphemeralRecipientFingerprint: fingerprint,
	}, nil
}

func (v *restoreVerifier) failureWith(started time.Time, fp string, err error) *meta.RestoreVerificationResult {
	return verifier.FailureResult(v.Mode(), started, err, fp)
}

func (v *restoreVerifier) mismatchWith(started time.Time, fp, summary string) *meta.RestoreVerificationResult {
	now := time.Now().UTC()
	return &meta.RestoreVerificationResult{
		Mode:                          v.Mode(),
		Verdict:                       meta.VerificationMismatch,
		Summary:                       summary,
		StartedAt:                     started,
		CompletedAt:                   now,
		DurationSeconds:               now.Sub(started).Seconds(),
		EphemeralRecipientFingerprint: fp,
	}
}

// openDecryptedStream returns the plaintext (decrypted + gunzipped)
// reader for the dump file, plus a closer that releases all wrapping
// resources. Caller MUST defer-call closer().
func openDecryptedStream(path string, id *crypto.EphemeralIdentity) (io.Reader, func(), error) {
	noop := func() {}
	f, err := os.Open(path)
	if err != nil {
		return nil, noop, fmt.Errorf("open dump: %w", err)
	}
	if id == nil {
		_ = f.Close()
		return nil, noop, errors.New("ephemeral identity is nil")
	}
	dec, err := id.Decryptor()
	if err != nil {
		_ = f.Close()
		return nil, noop, fmt.Errorf("ephemeral decryptor: %w", err)
	}
	plain, err := dec.Wrap(f)
	if err != nil {
		_ = f.Close()
		return nil, noop, fmt.Errorf("age decrypt: %w", err)
	}
	gz, err := gzip.NewReader(plain)
	if err != nil {
		_ = f.Close()
		return nil, noop, fmt.Errorf("gunzip: %w", err)
	}
	closer := func() {
		_ = gz.Close()
		_ = f.Close()
	}
	return gz, closer, nil
}

// imageOverrideFromSource returns Source.VerificationImage, the
// first-class annotation that lets users pin the verifier-pod's image
// to match their source DB version. Empty → DefaultImage(dbType).
func imageOverrideFromSource(s *secrets.Source) string {
	if s == nil {
		return ""
	}
	return s.VerificationImage
}

// resolveVolumeBytes picks the emptyDir size limit for the verifier
// pod. Per-mode defaults reflect what the engine actually needs:
// schema-only barely touches disk, full needs ~2-3x source size. When
// the source has an explicit VerificationVolumeSize override (e.g.
// "100Gi"), it wins.
func resolveVolumeBytes(s *secrets.Source, mode Mode) int64 {
	if s != nil && s.VerificationVolumeSize != "" {
		if v, ok := parseSizeBytes(s.VerificationVolumeSize); ok {
			return v
		}
	}
	switch mode {
	case ModeSchemaOnly:
		return 1 * 1024 * 1024 * 1024 // 1 GiB
	case ModeSample:
		return 5 * 1024 * 1024 * 1024 // 5 GiB
	case ModeFull:
		return 50 * 1024 * 1024 * 1024 // 50 GiB
	}
	return 5 * 1024 * 1024 * 1024
}

// parseSizeBytes accepts simple suffixes Ki, Mi, Gi, Ti and the
// decimal K, M, G, T variants. Falls back to (0, false) on
// unrecognised input — the caller then uses the per-mode default.
func parseSizeBytes(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	mult := int64(1)
	for _, suf := range []struct {
		s string
		m int64
	}{
		{"Ti", 1 << 40},
		{"Gi", 1 << 30},
		{"Mi", 1 << 20},
		{"Ki", 1 << 10},
		{"T", 1_000_000_000_000},
		{"G", 1_000_000_000},
		{"M", 1_000_000},
		{"K", 1_000},
	} {
		if strings.HasSuffix(s, suf.s) {
			mult = suf.m
			s = s[:len(s)-len(suf.s)]
			break
		}
	}
	var n int64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil || n < 0 {
		return 0, false
	}
	return n * mult, true
}

// totalsByTable extracts per-table row counts from preStats. Returns
// nil for nil input — engines no-op the comparison loop on nil.
func totalsByTable(s *dumper.Stats) map[string]int64 {
	if s == nil {
		return nil
	}
	out := make(map[string]int64, len(s.Tables))
	for _, t := range s.Tables {
		out[t.Name] = t.RowCount
	}
	return out
}

// evaluateSmoke condenses the per-table SmokeResult into a verdict.
func evaluateSmoke(mode Mode, sr *SmokeResult) (string, string) {
	if sr == nil || len(sr.Tables) == 0 {
		notes := ""
		if sr != nil && len(sr.Notes) > 0 {
			notes = " — " + strings.Join(sr.Notes, "; ")
		}
		return meta.VerificationSkipped, fmt.Sprintf("restore + reachability OK; no smoke comparisons performed%s", notes)
	}
	var matches, mismatches int
	for _, t := range sr.Tables {
		if t.Match {
			matches++
		} else {
			mismatches++
		}
	}
	if mismatches == 0 {
		return meta.VerificationMatch, fmt.Sprintf("restore + smoke OK (%d/%d tables matched)", matches, len(sr.Tables))
	}
	return meta.VerificationMismatch, fmt.Sprintf("smoke mismatch on %d/%d tables", mismatches, len(sr.Tables))
}
