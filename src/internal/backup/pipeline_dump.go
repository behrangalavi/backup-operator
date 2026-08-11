package backup

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"backup-operator/crypto"
	"backup-operator/dumper"
	"backup-operator/internal/labels"

	"github.com/klauspost/compress/zstd"
)

// newCompressor returns a WriteCloser that compresses data written to it
// using the specified algorithm ("gzip" or "zstd").
func newCompressor(w io.Writer, compression string) (io.WriteCloser, error) {
	if compression == labels.CompressionZstd {
		return zstd.NewWriter(w)
	}
	return gzip.NewWriter(w), nil
}

// dumpToFileWithEncryptor dumps the database to a temp file while optionally
// counting rows via the RowCounter, using the supplied Encryptor. Restore-
// verification supplies a per-run encryptor that includes an extra ephemeral
// recipient; everything else passes p.encryptor.
// dumpResult carries the sizes the verification/analyzer layers need. Encrypted
// is the on-disk (age + compression) size; Raw is the uncompressed, unencrypted
// dump byte count captured at the tee point — the latter is the only size that
// can tell a header-only dump from a real one (age+gzip overhead alone is a few
// hundred bytes, so the encrypted size can never distinguish them).
type dumpResult struct {
	EncryptedSize int64
	RawSize       int64
	SHA256        string
}

func (p *Pipeline) dumpToFileWithEncryptor(ctx context.Context, d dumper.Dumper, dumpFile string, rc *dumper.RowCounter, encryptor crypto.Encryptor, compression string) (dumpResult, error) {
	f, err := os.Create(dumpFile)
	if err != nil {
		return dumpResult{}, fmt.Errorf("create temp dump: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	w := io.MultiWriter(f, h)

	enc, err := encryptor.Wrap(w)
	if err != nil {
		return dumpResult{}, fmt.Errorf("encrypt wrap: %w", err)
	}
	cw, err := newCompressor(enc, compression)
	if err != nil {
		_ = enc.Close()
		return dumpResult{}, fmt.Errorf("compressor init: %w", err)
	}

	// The row counter sits before compression so it sees raw dump output.
	var dumpWriter io.Writer = cw
	if rc != nil {
		rc.SetWriter(cw)
		dumpWriter = rc
	}
	// Count raw (pre-compression, pre-encryption) dump bytes for empty-dump
	// detection on engines the row counter can't parse (mongo/redis).
	counted := &countingWriter{w: dumpWriter}

	if err := d.Dump(ctx, counted); err != nil {
		if rc != nil {
			_ = rc.Close()
		}
		_ = cw.Close()
		_ = enc.Close()
		return dumpResult{}, err
	}
	if rc != nil {
		_ = rc.Close()
	}
	if err := cw.Close(); err != nil {
		_ = enc.Close()
		return dumpResult{}, fmt.Errorf("compressor close: %w", err)
	}
	if err := enc.Close(); err != nil {
		return dumpResult{}, fmt.Errorf("age close: %w", err)
	}

	info, err := f.Stat()
	if err != nil {
		return dumpResult{}, err
	}
	return dumpResult{EncryptedSize: info.Size(), RawSize: counted.n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// countingWriter tallies bytes passed through to the wrapped writer.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
