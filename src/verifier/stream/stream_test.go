package stream

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"backup-operator/crypto"
	"backup-operator/dumper"
	"backup-operator/internal/labels"
	"backup-operator/internal/meta"
	"backup-operator/internal/secrets"
	"backup-operator/verifier"

	"github.com/go-logr/logr/testr"
	"github.com/klauspost/compress/zstd"
)

// makeEncryptedDump writes a temp file containing
// gzip(plaintext) sealed with the ephemeral identity, returning its path
// plus the identity. Mirrors what the pipeline writes at run time.
func makeEncryptedDump(t *testing.T, plaintext []byte) (string, *crypto.EphemeralIdentity) {
	t.Helper()

	eph, err := crypto.GenerateEphemeralIdentity()
	if err != nil {
		t.Fatal(err)
	}
	enc, err := crypto.NewEncryptorWithExtraRecipient(noopEncryptor(t), eph.PublicLine())
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	dumpPath := filepath.Join(dir, "dump.sql.gz.age")
	f, err := os.Create(dumpPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	wc, err := enc.Wrap(f)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(wc)
	if _, err := gz.Write(plaintext); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := wc.Close(); err != nil {
		t.Fatal(err)
	}
	return dumpPath, eph
}

// noopEncryptor builds a base encryptor with a known DR key so
// NewEncryptorWithExtraRecipient has something to layer on top of.
func noopEncryptor(t *testing.T) crypto.Encryptor {
	t.Helper()
	const drPub = "age1g5hdv6wq0fgph462wpwtgm44vhjjex9xam27s0qsrhwzrfmyxcrs59qd48"
	enc, err := crypto.NewFromPublicKeys(drPub)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

func TestStream_PostgresMatch(t *testing.T) {
	plain := []byte(`-- PostgreSQL database dump
SET statement_timeout = 0;
COPY public.users (id, name) FROM stdin;
1	alice
2	bob
3	charlie
\.
COPY public.orders (id, ts) FROM stdin;
10	2024-01-01
\.
`)

	dumpPath, eph := makeEncryptedDump(t, plain)
	defer eph.Wipe()

	v := New("postgres", testr.New(t))
	res, err := v.Verify(context.Background(), verifier.Input{
		Source:    &secrets.Source{TargetName: "x", DBType: "postgres"},
		DumpPath:  dumpPath,
		Identity:  eph,
		PreStats:  &dumper.Stats{Tables: []dumper.TableStats{{Name: "public.users", RowCount: 3}, {Name: "public.orders", RowCount: 1}}},
		StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Verdict != meta.VerificationMatch {
		t.Errorf("verdict = %q, want match. summary=%q", res.Verdict, res.Summary)
	}
	if res.Mode != labels.RestoreVerificationStreamValidate {
		t.Errorf("mode = %q", res.Mode)
	}
	if res.EphemeralRecipientFingerprint == "" {
		t.Error("fingerprint must be populated")
	}
}

func TestStream_PostgresEmptyDumpMismatch(t *testing.T) {
	plain := []byte(`-- PostgreSQL database dump
-- DDL only, no COPY
`)
	dumpPath, eph := makeEncryptedDump(t, plain)
	defer eph.Wipe()

	v := New("postgres", testr.New(t))
	res, err := v.Verify(context.Background(), verifier.Input{
		Source:    &secrets.Source{TargetName: "x", DBType: "postgres"},
		DumpPath:  dumpPath,
		Identity:  eph,
		PreStats:  &dumper.Stats{Tables: []dumper.TableStats{{Name: "public.users", RowCount: 100}}},
		StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Verdict != meta.VerificationMismatch {
		t.Errorf("verdict = %q, want mismatch. summary=%q", res.Verdict, res.Summary)
	}
}

func TestStream_PostgresHeaderInvalid(t *testing.T) {
	// Plaintext without a SQL comment header at all.
	plain := []byte("garbage data not a SQL dump\n")
	dumpPath, eph := makeEncryptedDump(t, plain)
	defer eph.Wipe()

	v := New("postgres", testr.New(t))
	res, _ := v.Verify(context.Background(), verifier.Input{
		Source:    &secrets.Source{TargetName: "x", DBType: "postgres"},
		DumpPath:  dumpPath,
		Identity:  eph,
		PreStats:  &dumper.Stats{},
		StartedAt: time.Now(),
	})
	if res.Verdict != meta.VerificationMismatch {
		t.Errorf("verdict = %q, want mismatch. summary=%q", res.Verdict, res.Summary)
	}
}

func TestStream_MySQLMatch(t *testing.T) {
	plain := []byte(`-- MySQL dump 10.13
INSERT INTO ` + "`users`" + ` VALUES (1,'a'),(2,'b'),(3,'c');
INSERT INTO ` + "`orders`" + ` VALUES (10,'2024');
`)
	dumpPath, eph := makeEncryptedDump(t, plain)
	defer eph.Wipe()

	v := New("mysql", testr.New(t))
	res, _ := v.Verify(context.Background(), verifier.Input{
		Source:    &secrets.Source{TargetName: "x", DBType: "mysql"},
		DumpPath:  dumpPath,
		Identity:  eph,
		PreStats:  &dumper.Stats{Tables: []dumper.TableStats{{Name: "users", RowCount: 3}, {Name: "orders", RowCount: 1}}},
		StartedAt: time.Now(),
	})
	if res.Verdict != meta.VerificationMatch {
		t.Errorf("verdict = %q, want match. summary=%q", res.Verdict, res.Summary)
	}
}

func TestStream_MongoMagicMatch(t *testing.T) {
	// Build a minimal "valid" mongodump archive: 4-byte LE magic + some bytes.
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0x8199e26d))
	buf.WriteString("rest of archive (not parsed)")
	dumpPath, eph := makeEncryptedDump(t, buf.Bytes())
	defer eph.Wipe()

	v := New("mongo", testr.New(t))
	res, _ := v.Verify(context.Background(), verifier.Input{
		Source:    &secrets.Source{TargetName: "x", DBType: "mongo"},
		DumpPath:  dumpPath,
		Identity:  eph,
		StartedAt: time.Now(),
	})
	if res.Verdict != meta.VerificationMatch {
		t.Errorf("verdict = %q, want match. summary=%q", res.Verdict, res.Summary)
	}
}

func TestStream_MongoMagicMismatch(t *testing.T) {
	plain := []byte("not a mongodump archive")
	dumpPath, eph := makeEncryptedDump(t, plain)
	defer eph.Wipe()

	v := New("mongo", testr.New(t))
	res, _ := v.Verify(context.Background(), verifier.Input{
		Source:    &secrets.Source{TargetName: "x", DBType: "mongo"},
		DumpPath:  dumpPath,
		Identity:  eph,
		StartedAt: time.Now(),
	})
	if res.Verdict != meta.VerificationMismatch {
		t.Errorf("verdict = %q, want mismatch. summary=%q", res.Verdict, res.Summary)
	}
}

func TestStream_RedisMatch(t *testing.T) {
	plain := append([]byte("REDIS0011"), []byte("body bytes...")...)
	dumpPath, eph := makeEncryptedDump(t, plain)
	defer eph.Wipe()

	v := New("redis", testr.New(t))
	res, _ := v.Verify(context.Background(), verifier.Input{
		Source:    &secrets.Source{TargetName: "x", DBType: "redis"},
		DumpPath:  dumpPath,
		Identity:  eph,
		StartedAt: time.Now(),
	})
	if res.Verdict != meta.VerificationMatch {
		t.Errorf("verdict = %q, want match. summary=%q", res.Verdict, res.Summary)
	}
}

func TestStream_RedisBadMagic(t *testing.T) {
	plain := []byte("XYZIS0011...")
	dumpPath, eph := makeEncryptedDump(t, plain)
	defer eph.Wipe()

	v := New("redis", testr.New(t))
	res, _ := v.Verify(context.Background(), verifier.Input{
		Source:    &secrets.Source{TargetName: "x", DBType: "redis"},
		DumpPath:  dumpPath,
		Identity:  eph,
		StartedAt: time.Now(),
	})
	if res.Verdict != meta.VerificationMismatch {
		t.Errorf("verdict = %q, want mismatch. summary=%q", res.Verdict, res.Summary)
	}
}

func TestStream_DecryptFailsWhenIdentityWiped(t *testing.T) {
	plain := []byte("-- PostgreSQL database dump\n")
	dumpPath, eph := makeEncryptedDump(t, plain)

	// Wipe identity before verifier runs.
	eph.Wipe()

	v := New("postgres", testr.New(t))
	res, _ := v.Verify(context.Background(), verifier.Input{
		Source:    &secrets.Source{TargetName: "x", DBType: "postgres"},
		DumpPath:  dumpPath,
		Identity:  eph,
		StartedAt: time.Now(),
	})
	// Wiped identity → FailureResult path → Verdict skipped, Error set.
	if res.Verdict != meta.VerificationSkipped {
		t.Errorf("verdict = %q, want skipped (verifier failed). summary=%q error=%q", res.Verdict, res.Summary, res.Error)
	}
	if res.Error == "" {
		t.Error("Error must be populated on verifier failure")
	}
}

// makeEncryptedDumpZstd is like makeEncryptedDump but uses zstd compression.
func makeEncryptedDumpZstd(t *testing.T, plaintext []byte) (string, *crypto.EphemeralIdentity) {
	t.Helper()

	eph, err := crypto.GenerateEphemeralIdentity()
	if err != nil {
		t.Fatal(err)
	}
	enc, err := crypto.NewEncryptorWithExtraRecipient(noopEncryptor(t), eph.PublicLine())
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	dumpPath := filepath.Join(dir, "dump.sql.gz.age")
	f, err := os.Create(dumpPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	wc, err := enc.Wrap(f)
	if err != nil {
		t.Fatal(err)
	}
	zw, err := zstd.NewWriter(wc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(plaintext); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := wc.Close(); err != nil {
		t.Fatal(err)
	}
	return dumpPath, eph
}

func TestStream_PostgresZstdMatch(t *testing.T) {
	plain := []byte(`-- PostgreSQL database dump
SET statement_timeout = 0;
COPY public.users (id, name) FROM stdin;
1	alice
2	bob
\.
`)
	dumpPath, eph := makeEncryptedDumpZstd(t, plain)
	defer eph.Wipe()

	v := New("postgres", testr.New(t))
	res, err := v.Verify(context.Background(), verifier.Input{
		Source:      &secrets.Source{TargetName: "x", DBType: "postgres"},
		DumpPath:    dumpPath,
		Identity:    eph,
		Compression: labels.CompressionZstd,
		PreStats:    &dumper.Stats{Tables: []dumper.TableStats{{Name: "public.users", RowCount: 2}}},
		StartedAt:   time.Now(),
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Verdict != meta.VerificationMatch {
		t.Errorf("verdict = %q, want match. summary=%q", res.Verdict, res.Summary)
	}
}

func TestStream_UnsupportedDBSkipped(t *testing.T) {
	plain := []byte("anything")
	dumpPath, eph := makeEncryptedDump(t, plain)
	defer eph.Wipe()

	v := New("clickhouse", testr.New(t))
	res, _ := v.Verify(context.Background(), verifier.Input{
		Source:    &secrets.Source{TargetName: "x", DBType: "clickhouse"},
		DumpPath:  dumpPath,
		Identity:  eph,
		StartedAt: time.Now(),
	})
	if res.Verdict != meta.VerificationSkipped {
		t.Errorf("verdict = %q, want skipped (unsupported dbType)", res.Verdict)
	}
}

// TestStream_OversizeLineFallsBackToMatch regresses the bug where a dump with a
// single line larger than the row-counter's 10 MB scanner buffer (a wide row
// with a big blob) produced a Close error -> Skipped -> permanent critical
// alert. The verifier must instead validate the header and pass with a
// "row count unavailable" note.
func TestStream_OversizeLineFallsBackToMatch(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("-- PostgreSQL database dump\n")
	b.WriteString("COPY public.blobs (id, data) FROM stdin;\n")
	b.WriteString("1\t")
	b.Write(bytes.Repeat([]byte("A"), 11*1024*1024)) // > 10 MB single line
	b.WriteString("\n\\.\n")

	dumpPath, eph := makeEncryptedDump(t, b.Bytes())
	defer eph.Wipe()

	v := New("postgres", testr.New(t))
	res, err := v.Verify(context.Background(), verifier.Input{
		Source:    &secrets.Source{TargetName: "x", DBType: "postgres"},
		DumpPath:  dumpPath,
		Identity:  eph,
		PreStats:  &dumper.Stats{Tables: []dumper.TableStats{{Name: "public.blobs", RowCount: 1}}},
		StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Verdict != meta.VerificationMatch {
		t.Errorf("verdict = %q, want match (oversize line must not be a critical). summary=%q", res.Verdict, res.Summary)
	}
}
