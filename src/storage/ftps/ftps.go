// Package ftps implements the storage.Storage interface for FTP over TLS.
// It exists because some NAS firmware (QNAP, older Synology, FreeNAS) only
// offers FTPS — not SFTP — and forcing operators to flip protocols on the
// device is often not an option for compliance-locked appliances.
//
// Two TLS modes are supported:
//
//   - "explicit" (default, port 21): connect in plaintext and upgrade via
//     AUTH TLS. This is what most NAS UIs label as "FTP with SSL/TLS
//     (explicit)" — and what RFC 4217 standardised.
//   - "implicit" (port 990): TLS is wrapped around the entire control
//     connection from byte zero. Older and less common, but still seen.
//
// The data channel is always upgraded to TLS too (PROT P). Plain control +
// plain data is intentionally NOT exposed — if the user picked FTPS they
// want everything encrypted; mixed-mode is a footgun, not a feature.
package ftps

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"path"
	"strconv"
	"strings"
	"time"

	"backup-operator/storage"

	"github.com/go-logr/logr"
	"github.com/jlaffaye/ftp"
)

// Required Secret keys: host, username, password.
// Optional: port (default 21 explicit / 990 implicit), tls-mode (explicit|implicit),
// insecure-skip-cert-verify, path-prefix.
const (
	keyHost                   = "host"
	keyPort                   = "port"
	keyUsername               = "username"
	keyPassword               = "password"
	keyTLSMode                = "tls-mode"
	keyInsecureSkipCertVerify = "insecure-skip-cert-verify"
	keyPathPrefix             = "path-prefix"

	tlsModeExplicit = "explicit"
	tlsModeImplicit = "implicit"
)

// Compile-time interface checks.
var _ storage.Storage = (*ftpsStorage)(nil)
var _ storage.BatchStorage = (*ftpsStorage)(nil)

type ftpsStorage struct {
	name        string
	addr        string
	host        string
	user        string
	password    string
	implicitTLS bool
	tlsConfig   *tls.Config
	pathPrefix  string
	logger      logr.Logger
}

func New(name string, data storage.SecretData, logger logr.Logger) (storage.Storage, error) {
	host := strings.TrimSpace(string(data[keyHost]))
	if host == "" {
		return nil, fmt.Errorf("ftps storage %q: missing %q", name, keyHost)
	}
	user := strings.TrimSpace(string(data[keyUsername]))
	if user == "" {
		return nil, fmt.Errorf("ftps storage %q: missing %q", name, keyUsername)
	}
	// Trim trailing CR/LF only — paste-from-password-manager artifact. Real
	// passwords can have leading/trailing spaces, so we deliberately keep
	// TrimSpace away from this. See the SFTP driver for the same reasoning.
	password := strings.TrimRight(string(data[keyPassword]), "\r\n")
	if password == "" {
		return nil, fmt.Errorf("ftps storage %q: missing %q", name, keyPassword)
	}

	mode := strings.ToLower(strings.TrimSpace(string(data[keyTLSMode])))
	if mode == "" {
		mode = tlsModeExplicit
	}
	if mode != tlsModeExplicit && mode != tlsModeImplicit {
		return nil, fmt.Errorf("ftps storage %q: invalid %q %q (must be %q or %q)",
			name, keyTLSMode, mode, tlsModeExplicit, tlsModeImplicit)
	}
	implicit := mode == tlsModeImplicit

	port := 21
	if implicit {
		port = 990
	}
	if p := strings.TrimSpace(string(data[keyPort])); p != "" {
		parsed, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("ftps storage %q: invalid port %q: %w", name, p, err)
		}
		port = parsed
	}

	skipVerify := strings.EqualFold(strings.TrimSpace(string(data[keyInsecureSkipCertVerify])), "true")
	if skipVerify {
		logger.Info("INSECURE: skipping TLS certificate verification for FTPS destination", "storage", name)
	}

	return &ftpsStorage{
		name:        name,
		addr:        net.JoinHostPort(host, strconv.Itoa(port)),
		host:        host,
		user:        user,
		password:    password,
		implicitTLS: implicit,
		tlsConfig: &tls.Config{
			ServerName:         host,
			InsecureSkipVerify: skipVerify, //nolint:gosec // opt-in via Secret data; warned in logs
			MinVersion:         tls.VersionTLS12,
			// ClientSessionCache lets the data channel resume the control
			// channel's TLS session. QNAP, Pure-FTPd, vsftpd's "require_ssl_reuse"
			// (default on), and most other strict FTPS servers reject a fresh
			// data-channel handshake as a session-hijacking countermeasure —
			// without resumption, login succeeds but every STOR/RETR fails as
			// soon as PASV opens the data socket. The cache is per-instance
			// so each destination gets its own session pool.
			ClientSessionCache: tls.NewLRUClientSessionCache(32),
		},
		pathPrefix: strings.TrimRight(string(data[keyPathPrefix]), "/"),
		logger:     logger,
	}, nil
}

func (s *ftpsStorage) Name() string { return s.name }

// dial opens a fresh FTPS control connection and authenticates. The PROT P
// command is sent so the data channel is encrypted too — without it the FTP
// server would happily serve dumps over plaintext data sockets, which would
// defeat the point of using FTPS.
func (s *ftpsStorage) dial(ctx context.Context) (*ftp.ServerConn, error) {
	deadline, ok := ctx.Deadline()
	timeout := 30 * time.Second
	if ok {
		if d := time.Until(deadline); d > 0 && d < timeout {
			timeout = d
		}
	}

	opts := []ftp.DialOption{
		ftp.DialWithTimeout(timeout),
		ftp.DialWithContext(ctx),
		ftp.DialWithTLS(s.tlsConfig),
	}
	if !s.implicitTLS {
		// Explicit FTPS: connect plain, then AUTH TLS upgrade.
		opts = []ftp.DialOption{
			ftp.DialWithTimeout(timeout),
			ftp.DialWithContext(ctx),
			ftp.DialWithExplicitTLS(s.tlsConfig),
		}
	}

	c, err := ftp.Dial(s.addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", s.addr, err)
	}
	if err := c.Login(s.user, s.password); err != nil {
		_ = c.Quit()
		return nil, fmt.Errorf("login: %w", err)
	}
	return c, nil
}

func (s *ftpsStorage) full(p string) string {
	if s.pathPrefix == "" {
		return p
	}
	return path.Join(s.pathPrefix, p)
}

// stripPrefix turns a full server path back into the logical (prefix-less)
// path callers passed to Upload — Object.Path round-trips through Get/Delete.
func (s *ftpsStorage) stripPrefix(full string) string {
	if s.pathPrefix == "" {
		return full
	}
	rel := strings.TrimPrefix(full, s.pathPrefix)
	return strings.TrimPrefix(rel, "/")
}

// ftpDirClient is the subset of *ftp.ServerConn mkdirAll needs. Narrowing to
// an interface keeps the CWD-restore logic unit-testable without a live FTP
// server.
type ftpDirClient interface {
	MakeDir(path string) error
	ChangeDir(path string) error
	CurrentDir() (string, error)
}

// mkdirAll creates each segment in turn. jlaffaye/ftp's MakeDir is
// single-level only — the protocol has no mkdir -p equivalent. We can't
// reliably distinguish "already exists" from "permission denied" at the
// MakeDir level (both come back as 550), so on failure a ChangeDir probes
// whether the directory is actually usable: success means it exists (probably
// why MakeDir failed); failure surfaces the real reason — missing parent, no
// permission, chroot violation — instead of letting Stor fail later with a
// misleading "no such file".
//
// ChangeDir MUTATES the session CWD, and every path here is a full path
// resolved against the login directory. So the probe MUST restore the CWD
// immediately: a probe that left the CWD inside the tree would make the next
// MakeDir target <cwd>/<full-path> (a doubled prefix) and fail — which is
// exactly what broke meta uploads on every FTPS destination whenever the
// path-prefix was relative or empty (an absolute prefix masked it, since
// absolute paths don't depend on CWD). If the server doesn't support PWD we
// cannot restore, so we skip the probe and treat 550 as "probably exists",
// letting Stor surface any real problem later.
func mkdirAll(c ftpDirClient, dir string) error {
	if dir == "" || dir == "/" || dir == "." {
		return nil
	}
	parent := path.Dir(dir)
	if parent != dir && parent != "/" && parent != "." {
		if err := mkdirAll(c, parent); err != nil {
			return err
		}
	}
	mkErr := c.MakeDir(dir)
	if mkErr == nil {
		return nil
	}
	cwd, cwdErr := c.CurrentDir()
	if cwdErr != nil {
		// Can't safely restore the CWD without knowing it — skip the probe
		// rather than risk corrupting the session for subsequent operations.
		return nil
	}
	cdErr := c.ChangeDir(dir)
	_ = c.ChangeDir(cwd) // restore regardless of probe outcome
	if cdErr != nil {
		return fmt.Errorf("mkdir %s: %w (cwd probe: %v)", dir, mkErr, cdErr)
	}
	return nil
}

func (s *ftpsStorage) Upload(ctx context.Context, p string, r io.Reader) error {
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Quit() }()

	full := s.full(p)
	if err := mkdirAll(c, path.Dir(full)); err != nil {
		return err
	}

	if err := c.Stor(full, r); err != nil {
		// Best-effort cleanup of any partial file the server may have created.
		_ = c.Delete(full)
		return fmt.Errorf("store %s: %w", full, err)
	}
	return nil
}

func (s *ftpsStorage) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	c, err := s.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Quit() }()

	root := s.full(prefix)
	return walkList(ctx, c, root, s.stripPrefix)
}

// walkList recursively lists files under root using FTP's LIST. Servers vary
// in what they return (Unix-style, DOS-style, MLSD when available); jlaffaye
// abstracts that and gives us *ftp.Entry values with Type set to File/Folder.
//
// Error policy: "directory does not exist" (550 No such file) is the
// expected first-run condition for a fresh target and is swallowed
// silently — retention would otherwise fail on a brand-new backup target.
// Every other error is propagated so the operator/worker can surface it
// (permission denied, connection drop, TLS data-channel failure, …)
// instead of falsely reporting an empty directory.
func walkList(ctx context.Context, c *ftp.ServerConn, root string, strip func(string) string) ([]storage.Object, error) {
	var out []storage.Object
	var walk func(dir string, isRoot bool) error
	walk = func(dir string, isRoot bool) error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("list cancelled: %w", err)
		}
		entries, err := c.List(dir)
		if err != nil {
			msg := err.Error()
			// 550 covers both "no such file" and "permission denied" in
			// the FTP spec — at the root we treat 550 as "target dir not
			// created yet" (legitimate fresh state for a brand-new backup
			// target). Below the root any 550 means a real problem worth
			// surfacing instead of silently returning an empty listing.
			if isRoot && (strings.Contains(msg, "550") || strings.Contains(msg, "No such") || strings.Contains(msg, "not found")) {
				return nil
			}
			return fmt.Errorf("list %s: %w", dir, err)
		}
		for _, e := range entries {
			if e.Name == "." || e.Name == ".." {
				continue
			}
			p := path.Join(dir, e.Name)
			switch e.Type {
			case ftp.EntryTypeFolder:
				if err := walk(p, false); err != nil {
					return err
				}
			case ftp.EntryTypeFile:
				out = append(out, storage.Object{
					Path:         strip(p),
					Size:         int64(e.Size),
					LastModified: e.Time,
				})
			}
		}
		return nil
	}
	if err := walk(root, true); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *ftpsStorage) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	c, err := s.dial(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := c.Retr(s.full(p))
	if err != nil {
		_ = c.Quit()
		return nil, fmt.Errorf("retr %s: %w", p, err)
	}
	return &ftpsReader{r: resp, c: c}, nil
}

type ftpsReader struct {
	r *ftp.Response
	c *ftp.ServerConn
}

func (r *ftpsReader) Read(p []byte) (int, error) { return r.r.Read(p) }
func (r *ftpsReader) Close() error {
	_ = r.r.Close()
	return r.c.Quit()
}

func (s *ftpsStorage) Delete(ctx context.Context, p string) error {
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Quit() }()
	if err := c.Delete(s.full(p)); err != nil {
		return fmt.Errorf("delete %s: %w", p, err)
	}
	return nil
}

// RemoveDirectory removes an empty directory. Mirrors the SFTP optional
// extension so retention can prune empty date partitions after deletes.
func (s *ftpsStorage) RemoveDirectory(ctx context.Context, p string) error {
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Quit() }()
	return c.RemoveDir(s.full(p))
}

// WithSession opens one FTPS connection and returns a Storage that reuses
// it for every call. The caller MUST call closer() when done. This
// prevents NAS firmware (QNAP, Synology) from triggering Network Access
// Protection IP-blocks during retention — without it, deleting 30 old
// dumps opens 30 separate TLS connections in rapid succession.
func (s *ftpsStorage) WithSession(ctx context.Context) (storage.Storage, func() error, error) {
	c, err := s.dial(ctx)
	if err != nil {
		return nil, nil, err
	}
	sess := &ftpsSession{parent: s, c: c}
	closer := func() error { return c.Quit() }
	return sess, closer, nil
}

// ftpsSession wraps a single FTPS connection so multiple List/Delete calls
// don't re-dial. Mirrors sftpSession in the sftp package.
type ftpsSession struct {
	parent *ftpsStorage
	c      *ftp.ServerConn
}

func (s *ftpsSession) Name() string { return s.parent.name }

func (s *ftpsSession) Upload(_ context.Context, p string, r io.Reader) error {
	full := s.parent.full(p)
	if err := mkdirAll(s.c, path.Dir(full)); err != nil {
		return err
	}
	if err := s.c.Stor(full, r); err != nil {
		_ = s.c.Delete(full)
		return fmt.Errorf("store %s: %w", full, err)
	}
	return nil
}

func (s *ftpsSession) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	return walkList(ctx, s.c, s.parent.full(prefix), s.parent.stripPrefix)
}

func (s *ftpsSession) Get(_ context.Context, p string) (io.ReadCloser, error) {
	resp, err := s.c.Retr(s.parent.full(p))
	if err != nil {
		return nil, fmt.Errorf("retr %s: %w", p, err)
	}
	// Don't wrap with ftpsReader — the session owns the connection.
	return resp, nil
}

func (s *ftpsSession) Delete(_ context.Context, p string) error {
	if err := s.c.Delete(s.parent.full(p)); err != nil {
		return fmt.Errorf("delete %s: %w", p, err)
	}
	return nil
}

func (s *ftpsSession) RemoveDirectory(_ context.Context, p string) error {
	return s.c.RemoveDir(s.parent.full(p))
}
