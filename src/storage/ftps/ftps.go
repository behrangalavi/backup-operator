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
	password := string(data[keyPassword])
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

// mkdirAll creates each segment in turn. jlaffaye/ftp's MakeDir is
// single-level only — the protocol has no mkdir -p equivalent. Errors are
// intentionally swallowed: the FTP spec returns the same 550 for both "path
// already exists" and "permission denied", and we can't tell them apart
// without a follow-up CWD. If the directory is genuinely uncreatable, the
// subsequent Stor will fail with a clearer error and the caller surfaces it.
func mkdirAll(c *ftp.ServerConn, dir string) {
	if dir == "" || dir == "/" || dir == "." {
		return
	}
	parent := path.Dir(dir)
	if parent != dir && parent != "/" && parent != "." {
		mkdirAll(c, parent)
	}
	_ = c.MakeDir(dir)
}

func (s *ftpsStorage) Upload(ctx context.Context, p string, r io.Reader) error {
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Quit() }()

	full := s.full(p)
	mkdirAll(c, path.Dir(full))

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
func walkList(ctx context.Context, c *ftp.ServerConn, root string, strip func(string) string) ([]storage.Object, error) {
	var out []storage.Object
	var walk func(dir string) error
	walk = func(dir string) error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("list cancelled: %w", err)
		}
		entries, err := c.List(dir)
		if err != nil {
			// Treat "directory does not exist" as empty rather than fatal —
			// retention runs against a fresh target produce this on first run.
			return nil
		}
		for _, e := range entries {
			if e.Name == "." || e.Name == ".." {
				continue
			}
			p := path.Join(dir, e.Name)
			switch e.Type {
			case ftp.EntryTypeFolder:
				if err := walk(p); err != nil {
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
	if err := walk(root); err != nil {
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
