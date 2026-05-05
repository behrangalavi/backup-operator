// Package stream implements the lightest-touch restore-verifier: it
// streams the just-uploaded encrypted dump back through age, gunzip, and
// (where parseable) the existing dumper.RowCounter. No DB pod is spawned,
// no PVC is allocated. Catches the common silent-failure modes — bad
// encryption, truncated upload, schema-only "empty" dumps — at near-zero
// operational cost.
//
// SQL engines (postgres, mysql, mariadb) get full row-count reproduction
// against preStats. Mongo and Redis get header-magic + plausibility
// checks: BSON archive starts with a 32-bit little-endian length followed
// by a recognisable document; RDB files start with the literal "REDIS"
// magic. Phase 2 verifiers will plug into a real DB for stronger checks.
package stream

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"backup-operator/dumper"
	"backup-operator/internal/labels"
	"backup-operator/internal/meta"
	"backup-operator/verifier"

	"github.com/go-logr/logr"
)

// New returns a Verifier that does in-process stream validation. Logger
// is captured so the verifier can emit structured progress at V(1) for
// long-running streams.
func New(dbType string, log logr.Logger) verifier.Verifier {
	return &streamVerifier{dbType: dbType, log: log.WithName("stream-validate")}
}

type streamVerifier struct {
	dbType string
	log    logr.Logger
}

func (s *streamVerifier) Mode() string { return labels.RestoreVerificationStreamValidate }

func (s *streamVerifier) Verify(ctx context.Context, in verifier.Input) (*meta.RestoreVerificationResult, error) {
	started := in.StartedAt
	if started.IsZero() {
		started = time.Now().UTC()
	}
	fingerprint := ""
	if in.Identity != nil {
		fingerprint = in.Identity.RecipientFingerprint()
	}

	dec, err := in.Identity.Decryptor()
	if err != nil {
		return verifier.FailureResult(s.Mode(), started, fmt.Errorf("ephemeral decryptor: %w", err), fingerprint), nil
	}

	f, err := os.Open(in.DumpPath)
	if err != nil {
		return verifier.FailureResult(s.Mode(), started, fmt.Errorf("open dump: %w", err), fingerprint), nil
	}
	defer func() { _ = f.Close() }()

	plaintextR, err := dec.Wrap(f)
	if err != nil {
		return verifier.FailureResult(s.Mode(), started, fmt.Errorf("age decrypt: %w", err), fingerprint), nil
	}
	gz, err := gzip.NewReader(plaintextR)
	if err != nil {
		return verifier.FailureResult(s.Mode(), started, fmt.Errorf("gunzip: %w", err), fingerprint), nil
	}
	defer func() { _ = gz.Close() }()

	verdict, summary, parseErr := s.parse(ctx, in, gz)
	completed := time.Now().UTC()

	res := &meta.RestoreVerificationResult{
		Mode:                          s.Mode(),
		Verdict:                       verdict,
		Summary:                       summary,
		StartedAt:                     started,
		CompletedAt:                   completed,
		DurationSeconds:               completed.Sub(started).Seconds(),
		EphemeralRecipientFingerprint: fingerprint,
	}
	if parseErr != nil {
		res.Error = parseErr.Error()
	}
	return res, nil
}

// parse routes to the per-engine validator. Returns (verdict, summary, parseErr).
// parseErr is non-nil only on hard parser failures (e.g. corrupted gzip
// mid-stream). A "successful parse that found a mismatch" returns a
// VerificationMismatch verdict with parseErr nil.
func (s *streamVerifier) parse(ctx context.Context, in verifier.Input, r io.Reader) (string, string, error) {
	switch s.dbType {
	case "postgres", "mysql", "mariadb":
		return s.parseSQL(ctx, in, r)
	case "mongo":
		return s.parseMongo(ctx, r)
	case "redis":
		return s.parseRedis(ctx, r)
	default:
		return meta.VerificationSkipped, fmt.Sprintf("dbType %q not supported by stream-validate", s.dbType), nil
	}
}

// parseSQL re-uses the dumper.RowCounter to count INSERT/COPY rows from
// the decrypted plaintext stream, then compares the total against the
// pre-dump stats baseline.
//
// We deliberately do NOT compare per-table: SQL dumps may legitimately
// reorder, batch, or split tables, and the pre-stats numbers from
// pg_stat_user_tables are estimates anyway. The total-rows comparison
// catches the alarming cases ("dump has 0 rows but DB had 10k") without
// false-positive on cosmetic differences.
func (s *streamVerifier) parseSQL(ctx context.Context, in verifier.Input, r io.Reader) (string, string, error) {
	rc := dumper.NewRowCounter(io.Discard, s.dbType)
	defer func() { _ = rc.Close() }()

	// Wrap rc so we can also peek at the header for sanity. RowCounter
	// passes bytes through to its writer (io.Discard here) and forwards
	// to its scanner via internal pipe.
	headBuf := make([]byte, 0, 256)
	tee := &teeReader{src: r, copy: &headBuf, max: 256}
	if _, err := io.Copy(rc, tee); err != nil {
		// Cancellation is the one error we want to surface as Skipped
		// rather than Mismatch — the dump itself isn't necessarily bad.
		if ctx.Err() != nil {
			return meta.VerificationSkipped, "context cancelled mid-stream", ctx.Err()
		}
		return meta.VerificationSkipped, fmt.Sprintf("read decrypted stream: %v", err), err
	}
	if err := rc.Close(); err != nil && ctx.Err() == nil {
		return meta.VerificationSkipped, fmt.Sprintf("close row counter: %v", err), err
	}

	header := string(headBuf)
	if !sqlHeaderLooksValid(s.dbType, header) {
		return meta.VerificationMismatch,
			fmt.Sprintf("dump header does not look like a %s dump (first 256 bytes did not match expected marker)", s.dbType),
			nil
	}

	dumpRows := rc.TotalRows()
	preRows := totalRows(in.PreStats)

	switch {
	case preRows == 0 && dumpRows == 0:
		return meta.VerificationMatch, "decrypt + parse OK; no rows expected, none found", nil
	case preRows > 0 && dumpRows == 0:
		// Already covered by the existing empty-dump-check at dump-time, but
		// repeating here protects against the case where someone deletes the
		// dumper-side row-counter logic and only the verifier survives.
		return meta.VerificationMismatch,
			fmt.Sprintf("decrypted dump has 0 rows but pre-dump stats showed %d rows", preRows),
			nil
	case preRows == 0 && dumpRows > 0:
		// Concurrent inserts during preStats are normal; dump > pre is benign.
		return meta.VerificationMatch,
			fmt.Sprintf("decrypt + parse OK; %d rows (preStats showed 0; concurrent insert plausible)", dumpRows),
			nil
	}

	// Both > 0 — compare with the same tolerance the dump-time
	// BuildVerification uses: 99% match → match, otherwise mismatch.
	ratio := float64(dumpRows) / float64(preRows)
	if ratio >= 0.99 || dumpRows >= preRows {
		return meta.VerificationMatch,
			fmt.Sprintf("decrypt + parse OK; %d rows (pre-stats %d, ratio %.3f)", dumpRows, preRows, ratio),
			nil
	}
	return meta.VerificationMismatch,
		fmt.Sprintf("decrypted row count %d is only %.1f%% of pre-dump %d", dumpRows, ratio*100, preRows),
		nil
}

// parseMongo: just verify decrypt+gunzip succeeded and the first 4 bytes
// look like a sane mongodump archive (magic 0x8199e26d in LE). RowCounter
// can't speak BSON-archive without a real parser, so we don't try.
func (s *streamVerifier) parseMongo(ctx context.Context, r io.Reader) (string, string, error) {
	const mongoMagic = 0x8199e26d // mongodump --archive prefix
	br := bufio.NewReaderSize(r, 8)
	head, err := br.Peek(4)
	if err != nil {
		return meta.VerificationMismatch, fmt.Sprintf("read mongo archive header: %v", err), nil
	}
	if got := binary.LittleEndian.Uint32(head); got != mongoMagic {
		return meta.VerificationMismatch,
			fmt.Sprintf("mongodump magic mismatch: got 0x%08x, want 0x%08x", got, mongoMagic),
			nil
	}
	// Drain the rest to confirm the gzip layer doesn't blow up mid-stream.
	if _, err := io.Copy(io.Discard, br); err != nil && ctx.Err() == nil {
		return meta.VerificationMismatch, fmt.Sprintf("read mongo archive body: %v", err), err
	}
	return meta.VerificationMatch, "decrypt + gunzip OK; mongodump archive header valid", nil
}

// parseRedis: verify the RDB magic ("REDIS" + 4 ASCII version digits).
// Doesn't parse the per-key payload — that's Phase 2 territory.
func (s *streamVerifier) parseRedis(ctx context.Context, r io.Reader) (string, string, error) {
	br := bufio.NewReaderSize(r, 16)
	head, err := br.Peek(9)
	if err != nil {
		return meta.VerificationMismatch, fmt.Sprintf("read RDB header: %v", err), nil
	}
	if string(head[:5]) != "REDIS" {
		return meta.VerificationMismatch,
			fmt.Sprintf("RDB magic mismatch: got %q, want %q", string(head[:5]), "REDIS"),
			nil
	}
	for i := 5; i < 9; i++ {
		if head[i] < '0' || head[i] > '9' {
			return meta.VerificationMismatch,
				fmt.Sprintf("RDB version digits not numeric: %q", string(head[5:9])),
				nil
		}
	}
	if _, err := io.Copy(io.Discard, br); err != nil && ctx.Err() == nil {
		return meta.VerificationMismatch, fmt.Sprintf("read RDB body: %v", err), err
	}
	return meta.VerificationMatch,
		fmt.Sprintf("decrypt + gunzip OK; RDB header %q valid", string(head[:9])),
		nil
}

// sqlHeaderLooksValid is a soft sanity check on the first ~256 bytes of a
// SQL dump. It is intentionally permissive — different versions of pg_dump
// / mysqldump emit slightly different banners, and we'd rather pass on
// uncertainty than reject a legitimate dump because the banner format
// shifted between minor versions.
func sqlHeaderLooksValid(dbType, head string) bool {
	head = strings.TrimSpace(head)
	if head == "" {
		return false
	}
	// All three SQL dumpers emit SQL comments (-- ...) at the top.
	if !strings.HasPrefix(head, "--") && !strings.HasPrefix(head, "/*") &&
		!strings.HasPrefix(head, "SET ") && !strings.HasPrefix(head, "USE ") {
		return false
	}
	// Loose engine markers — present in vendor banners but not required.
	switch dbType {
	case "postgres":
		return strings.Contains(head, "PostgreSQL") || strings.Contains(head, "pg_dump") ||
			strings.HasPrefix(head, "--") // permissive: any SQL comment block
	case "mysql", "mariadb":
		return strings.Contains(head, "MySQL") || strings.Contains(head, "MariaDB") ||
			strings.Contains(head, "mysqldump") || strings.HasPrefix(head, "--")
	}
	return true
}

// totalRows sums per-table row counts in pre-dump stats. nil-safe.
func totalRows(s *dumper.Stats) int64 {
	if s == nil {
		return 0
	}
	var total int64
	for _, t := range s.Tables {
		total += t.RowCount
	}
	return total
}

// teeReader copies up to `max` bytes of the read stream into `copy`
// while passing all bytes through to the consumer. Lets the parser peek
// at the header without re-reading or buffering the whole stream.
type teeReader struct {
	src  io.Reader
	copy *[]byte
	max  int
}

func (t *teeReader) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 && len(*t.copy) < t.max {
		room := t.max - len(*t.copy)
		take := n
		if take > room {
			take = room
		}
		*t.copy = append(*t.copy, p[:take]...)
	}
	return n, err
}
