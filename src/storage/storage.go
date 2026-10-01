package storage

import (
	"context"
	"errors"
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

// ErrNotFound is wrapped by Get when the object does not exist, so callers
// can tell "nothing there" from a transport failure with errors.Is. Delete
// never returns it: deleting a missing object is a no-op success on every
// backend (see Storage.Delete).
var ErrNotFound = errors.New("storage: object not found")

// IsNotFound reports whether err wraps ErrNotFound.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// Storage abstracts the upload destination. The pipeline encrypts before
// calling Upload, so implementations only see ciphertext bytes — they MUST
// NOT add their own encryption.
//
// Contract every implementation honours (enforced by storagetest.Run):
//   - List returns logical, prefix-stripped paths that round-trip through
//     Get and Delete; a List prefix ending in "/" never matches a sibling
//     prefix ("db/" must not return "db-archive/...").
//   - Get on a missing object returns an error wrapping ErrNotFound.
//   - Delete on a missing object returns nil. Retention and purge re-runs
//     after a partial failure must converge instead of failing on the
//     objects an earlier attempt already removed.
//   - Close releases long-lived client resources; it is a no-op for
//     backends that dial per call. Callers own the lifecycle (the pool
//     closes evicted clients); a Storage must not be used after Close.
type Storage interface {
	Name() string
	Upload(ctx context.Context, path string, r io.Reader) error
	List(ctx context.Context, prefix string) ([]Object, error)
	Get(ctx context.Context, path string) (io.ReadCloser, error)
	Delete(ctx context.Context, path string) error
	Close() error
}

// BatchStorage is an optional extension for Storage implementations that
// support connection reuse across multiple operations (e.g. SFTP). Callers
// should type-assert before using. The returned Storage shares one connection
// and the closer MUST be called when done. Closing the session Storage itself
// (Close) is a no-op; the closer owns the connection.
type BatchStorage interface {
	Storage
	WithSession(ctx context.Context) (session Storage, closer func() error, err error)
}

// DirectoryRemover is an optional extension for hierarchical backends (SFTP,
// FTPS) where deleting the last file in a date partition leaves an empty
// directory behind. Object stores have no directories and don't implement
// it; retention type-asserts and skips. Removing a non-empty or missing
// directory may return an error — callers treat it as best-effort.
type DirectoryRemover interface {
	RemoveDirectory(ctx context.Context, path string) error
}

// SecretData carries the raw `data` map of a destination Secret. Each
// Storage implementation parses the keys it needs in its constructor.
type SecretData = map[string][]byte

// ContextReader returns a reader that fails with ctx.Err() once ctx is done.
// Streaming libraries that take a plain io.Reader (sftp, ftp) have no other
// way to observe cancellation mid-upload, so a cancelled run would otherwise
// keep pushing a multi-GB dump until the connection dropped on its own.
func ContextReader(ctx context.Context, r io.Reader) io.Reader {
	return &ctxReader{ctx: ctx, r: r}
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
