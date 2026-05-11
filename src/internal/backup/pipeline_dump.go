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
func (p *Pipeline) dumpToFileWithEncryptor(ctx context.Context, d dumper.Dumper, dumpFile string, rc *dumper.RowCounter, encryptor crypto.Encryptor, compression string) (int64, string, error) {
	f, err := os.Create(dumpFile)
	if err != nil {
		return 0, "", fmt.Errorf("create temp dump: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	w := io.MultiWriter(f, h)

	enc, err := encryptor.Wrap(w)
	if err != nil {
		return 0, "", fmt.Errorf("encrypt wrap: %w", err)
	}
	cw, err := newCompressor(enc, compression)
	if err != nil {
		_ = enc.Close()
		return 0, "", fmt.Errorf("compressor init: %w", err)
	}

	// The row counter sits before compression so it sees raw dump output.
	var dumpWriter io.Writer = cw
	if rc != nil {
		rc.SetWriter(cw)
		dumpWriter = rc
	}

	if err := d.Dump(ctx, dumpWriter); err != nil {
		if rc != nil {
			_ = rc.Close()
		}
		_ = cw.Close()
		_ = enc.Close()
		return 0, "", err
	}
	if rc != nil {
		_ = rc.Close()
	}
	if err := cw.Close(); err != nil {
		_ = enc.Close()
		return 0, "", fmt.Errorf("compressor close: %w", err)
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
