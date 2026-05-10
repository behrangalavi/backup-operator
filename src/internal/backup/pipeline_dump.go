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
)

func (p *Pipeline) dumpToFile(ctx context.Context, d dumper.Dumper, dumpFile string) (int64, string, error) {
	return p.dumpToFileWithCounter(ctx, d, dumpFile, nil)
}

// dumpToFileWithCounter is a thin wrapper that uses the pipeline's default
// encryptor — kept for backwards compatibility with existing tests and
// callers that don't need a per-run encryptor.
func (p *Pipeline) dumpToFileWithCounter(ctx context.Context, d dumper.Dumper, dumpFile string, rc *dumper.RowCounter) (int64, string, error) {
	return p.dumpToFileWithEncryptor(ctx, d, dumpFile, rc, p.encryptor)
}

// dumpToFileWithEncryptor dumps the database to a temp file while optionally
// counting rows via the RowCounter, using the supplied Encryptor. Restore-
// verification supplies a per-run encryptor that includes an extra ephemeral
// recipient; everything else passes p.encryptor.
func (p *Pipeline) dumpToFileWithEncryptor(ctx context.Context, d dumper.Dumper, dumpFile string, rc *dumper.RowCounter, encryptor crypto.Encryptor) (int64, string, error) {
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
	gz := gzip.NewWriter(enc)

	// The row counter sits before gzip so it sees raw dump output.
	var dumpWriter io.Writer = gz
	if rc != nil {
		rc.SetWriter(gz)
		dumpWriter = rc
	}

	if err := d.Dump(ctx, dumpWriter); err != nil {
		if rc != nil {
			_ = rc.Close()
		}
		_ = gz.Close()
		_ = enc.Close()
		return 0, "", err
	}
	if rc != nil {
		_ = rc.Close()
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
