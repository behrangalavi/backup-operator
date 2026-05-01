package storage

import (
	"context"
	"io"
	"time"
)

// Object describes a single stored item — used by analyzer to find the
// previous backup's metadata file and by retention to enumerate candidates.
type Object struct {
	Path         string
	Size         int64
	LastModified time.Time
}

// Storage abstracts the upload destination. The pipeline encrypts before
// calling Upload, so implementations only see ciphertext bytes — they MUST
// NOT add their own encryption.
type Storage interface {
	Name() string
	Upload(ctx context.Context, path string, r io.Reader) error
	List(ctx context.Context, prefix string) ([]Object, error)
	Get(ctx context.Context, path string) (io.ReadCloser, error)
	Delete(ctx context.Context, path string) error
}

// BatchStorage is an optional extension for Storage implementations that
// support connection reuse across multiple operations (e.g. SFTP). Callers
// should type-assert before using. The returned Storage shares one connection
// and the closer MUST be called when done.
type BatchStorage interface {
	Storage
	WithSession(ctx context.Context) (session Storage, closer func() error, err error)
}

// SecretData carries the raw `data` map of a destination Secret. Each
// Storage implementation parses the keys it needs in its constructor.
type SecretData = map[string][]byte
