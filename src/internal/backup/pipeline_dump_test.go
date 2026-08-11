package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"backup-operator/analyzer"
	"backup-operator/dumper"
	"backup-operator/internal/labels"
	"backup-operator/internal/secrets"

	"github.com/go-logr/logr"
	"github.com/klauspost/compress/zstd"
)

// --- resolvePolicy tests ---

func TestResolvePolicy(t *testing.T) {
	p := &Pipeline{defaults: RetentionPolicy{Days: 30, MinKeep: 3}}

	cases := []struct {
		name        string
		days, keep  int
		wantDays    int
		wantMinKeep int
	}{
		{"both absent (-1) use defaults", -1, -1, 30, 3},
		{"explicit zero disables retention", 0, 0, 0, 0},
		{"override days only", 7, -1, 7, 3},
		{"override minkeep only", -1, 5, 30, 5},
		{"override both", 14, 10, 14, 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := &secrets.Source{RetentionDays: c.days, MinKeep: c.keep}
			got := p.resolvePolicy(src)
			if got.Days != c.wantDays || got.MinKeep != c.wantMinKeep {
				t.Errorf("got {Days:%d MinKeep:%d}, want {Days:%d MinKeep:%d}",
					got.Days, got.MinKeep, c.wantDays, c.wantMinKeep)
			}
		})
	}
}

// --- analyzerForSource tests ---

func TestAnalyzerForSource_DefaultWhenThresholdsAbsent(t *testing.T) {
	def := analyzer.NewAnalyzer()
	p := &Pipeline{analyzer: def}
	src := &secrets.Source{RowDropThreshold: -1, SizeDropThreshold: -1}
	if got := p.analyzerForSource(src); got != def {
		t.Error("expected the pipeline's default analyzer when both thresholds are absent")
	}
}

func TestAnalyzerForSource_CustomWhenThresholdSet(t *testing.T) {
	def := analyzer.NewAnalyzer()
	p := &Pipeline{analyzer: def}
	src := &secrets.Source{RowDropThreshold: 0.3, SizeDropThreshold: -1}
	if got := p.analyzerForSource(src); got == def {
		t.Error("expected a fresh analyzer when a threshold is overridden")
	}
}

// --- newCompressor tests ---

func TestNewCompressor_Gzip(t *testing.T) {
	var buf bytes.Buffer
	w, err := newCompressor(&buf, labels.CompressionGzip)
	if err != nil {
		t.Fatalf("newCompressor gzip: %v", err)
	}
	if _, err := w.Write([]byte("hello gzip")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	gr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	got, _ := io.ReadAll(gr)
	if string(got) != "hello gzip" {
		t.Errorf("roundtrip mismatch: got %q", got)
	}
}

func TestNewCompressor_Zstd(t *testing.T) {
	var buf bytes.Buffer
	w, err := newCompressor(&buf, labels.CompressionZstd)
	if err != nil {
		t.Fatalf("newCompressor zstd: %v", err)
	}
	if _, err := w.Write([]byte("hello zstd")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	zr, err := zstd.NewReader(&buf)
	if err != nil {
		t.Fatalf("zstd reader: %v", err)
	}
	defer zr.Close()
	got, _ := io.ReadAll(zr)
	if string(got) != "hello zstd" {
		t.Errorf("roundtrip mismatch: got %q", got)
	}
}

func TestNewCompressor_UnknownDefaultsToGzip(t *testing.T) {
	var buf bytes.Buffer
	w, err := newCompressor(&buf, "lz4-not-supported")
	if err != nil {
		t.Fatalf("unknown compression should fall back to gzip, got err: %v", err)
	}
	_, _ = w.Write([]byte("x"))
	_ = w.Close()
	if _, err := gzip.NewReader(&buf); err != nil {
		t.Errorf("unknown compression should produce gzip output: %v", err)
	}
}

// --- dumpToFileWithEncryptor tests ---

// passthroughEncryptor is a crypto.Encryptor that does no encryption — it
// returns the underlying writer so the test can inspect the compressed bytes
// (gzip/zstd) without an age private key.
type passthroughEncryptor struct{}

func (passthroughEncryptor) Wrap(w io.Writer) (io.WriteCloser, error) {
	return nopWriteCloser{w}, nil
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// fakeDumper writes a fixed payload and returns canned stats.
type fakeDumper struct {
	dbType  string
	payload string
	dumpErr error
}

func (f *fakeDumper) Type() string { return f.dbType }
func (f *fakeDumper) Dump(_ context.Context, w io.Writer) error {
	if f.dumpErr != nil {
		return f.dumpErr
	}
	_, err := io.WriteString(w, f.payload)
	return err
}
func (f *fakeDumper) CollectStats(_ context.Context) (*dumper.Stats, error) {
	return &dumper.Stats{}, nil
}

func TestDumpToFileWithEncryptor_GzipRoundtrip(t *testing.T) {
	p := &Pipeline{logger: logr.Discard()}
	dumpFile := filepath.Join(t.TempDir(), "dump.sql.gz.age")
	payload := "INSERT INTO users VALUES (1, 'alice');\n"
	d := &fakeDumper{dbType: "postgres", payload: payload}

	res, err := p.dumpToFileWithEncryptor(context.Background(), d, dumpFile, nil,
		passthroughEncryptor{}, labels.CompressionGzip)
	if err != nil {
		t.Fatalf("dumpToFileWithEncryptor: %v", err)
	}
	size, sum := res.EncryptedSize, res.SHA256
	if size <= 0 {
		t.Errorf("expected positive file size, got %d", size)
	}
	// Raw (pre-compression) size must equal the payload the dumper emitted.
	if res.RawSize != int64(len(payload)) {
		t.Errorf("raw size %d != payload %d", res.RawSize, len(payload))
	}
	if len(sum) != 64 {
		t.Errorf("expected 64-char hex sha256, got %d chars: %q", len(sum), sum)
	}
	if _, err := hex.DecodeString(sum); err != nil {
		t.Errorf("sha256 not valid hex: %v", err)
	}

	// File on disk holds gzip(payload) because the encryptor is passthrough.
	raw, err := os.ReadFile(dumpFile)
	if err != nil {
		t.Fatalf("read dump file: %v", err)
	}
	if int64(len(raw)) != size {
		t.Errorf("returned size %d != on-disk %d", size, len(raw))
	}
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("file should be gzip: %v", err)
	}
	got, _ := io.ReadAll(gr)
	if string(got) != payload {
		t.Errorf("decompressed dump mismatch: got %q want %q", got, payload)
	}
}

func TestDumpToFileWithEncryptor_RowCounterSeesRawStream(t *testing.T) {
	p := &Pipeline{logger: logr.Discard()}
	dumpFile := filepath.Join(t.TempDir(), "dump.sql.gz.age")
	// Two INSERTs the SQL row counter should observe pre-compression.
	payload := "INSERT INTO `users` VALUES (1);\nINSERT INTO `users` VALUES (2);\n"
	d := &fakeDumper{dbType: "mysql", payload: payload}
	rc := dumper.NewRowCounter(nil, "mysql")

	_, err := p.dumpToFileWithEncryptor(context.Background(), d, dumpFile, rc,
		passthroughEncryptor{}, labels.CompressionGzip)
	if err != nil {
		t.Fatalf("dumpToFileWithEncryptor: %v", err)
	}
	if !rc.Active() {
		t.Fatal("row counter should be active for a SQL dbType")
	}
	counts := rc.Counts()
	if counts["users"] != 2 {
		t.Errorf("row counter should see 2 INSERTs into users, got %v", counts)
	}
}

func TestDumpToFileWithEncryptor_DumpErrorPropagates(t *testing.T) {
	p := &Pipeline{logger: logr.Discard()}
	dumpFile := filepath.Join(t.TempDir(), "dump.sql.gz.age")
	d := &fakeDumper{dbType: "postgres", dumpErr: fmt.Errorf("pg_dump: connection refused")}

	_, err := p.dumpToFileWithEncryptor(context.Background(), d, dumpFile, nil,
		passthroughEncryptor{}, labels.CompressionGzip)
	if err == nil {
		t.Fatal("expected dump error to propagate")
	}
}
