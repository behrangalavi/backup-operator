package sftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"backup-operator/storage"

	"github.com/go-logr/logr"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Required Secret keys: host, username, and exactly one of ssh-private-key OR password.
// Optional: port (default 22), known-hosts, path-prefix.
const (
	keyHost                   = "host"
	keyPort                   = "port"
	keyUsername               = "username"
	keyPrivateKey             = "ssh-private-key"
	keyPassword               = "password"
	keyKnownHosts             = "known-hosts"
	keyPathPrefix             = "path-prefix"
	keyInsecureSkipHostVerify = "insecure-skip-host-verify"
)

type sftpStorage struct {
	name       string
	addr       string
	user       string
	auths      []ssh.AuthMethod
	hostKeyCB  ssh.HostKeyCallback
	pathPrefix string
	logger     logr.Logger
}

func New(name string, data storage.SecretData, logger logr.Logger) (storage.Storage, error) {
	host := strings.TrimSpace(string(data[keyHost]))
	if host == "" {
		return nil, fmt.Errorf("sftp storage %q: missing %q", name, keyHost)
	}
	user := strings.TrimSpace(string(data[keyUsername]))
	if user == "" {
		return nil, fmt.Errorf("sftp storage %q: missing %q", name, keyUsername)
	}
	auths, err := buildAuthMethods(name, data)
	if err != nil {
		return nil, err
	}

	port := 22
	if p := strings.TrimSpace(string(data[keyPort])); p != "" {
		parsed, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("sftp storage %q: invalid port %q: %w", name, p, err)
		}
		port = parsed
	}

	allowInsecure := strings.EqualFold(strings.TrimSpace(string(data[keyInsecureSkipHostVerify])), "true")
	hostKeyCB, err := buildHostKeyCallback(name, data[keyKnownHosts], allowInsecure, logger)
	if err != nil {
		return nil, fmt.Errorf("sftp storage %q: %w", name, err)
	}

	return &sftpStorage{
		name:       name,
		addr:       net.JoinHostPort(host, strconv.Itoa(port)),
		user:       user,
		auths:      auths,
		hostKeyCB:  hostKeyCB,
		pathPrefix: strings.TrimRight(string(data[keyPathPrefix]), "/"),
		logger:     logger,
	}, nil
}

// buildAuthMethods accepts a destination Secret's data map and returns the
// ssh.AuthMethod list. Public-key auth is preferred when both are supplied —
// the password is treated as a fallback, matching how openssh chains methods.
// Exactly one of the two must be present; neither is a hard error so a typo'd
// data key can't accidentally produce an unauthenticated config.
func buildAuthMethods(name string, data storage.SecretData) ([]ssh.AuthMethod, error) {
	pkBytes := data[keyPrivateKey]
	// Trim trailing newlines only, not surrounding whitespace. Real passwords
	// may legitimately have leading/trailing spaces, but a trailing CR/LF is
	// almost always an artifact of pasting from a password manager — the
	// resulting "secret\n" is silently rejected by the server with no usable
	// diagnostic ("attempted methods [...], no supported methods remain")
	// while the user swears the password "works manually" (because they typed
	// it without the newline).
	password := strings.TrimRight(string(data[keyPassword]), "\r\n")

	hasKey := len(pkBytes) > 0
	hasPwd := password != ""
	if !hasKey && !hasPwd {
		return nil, fmt.Errorf("sftp storage %q: missing auth — provide %q or %q",
			name, keyPrivateKey, keyPassword)
	}

	var auths []ssh.AuthMethod
	if hasKey {
		signer, err := ssh.ParsePrivateKey(pkBytes)
		if err != nil {
			return nil, fmt.Errorf("sftp storage %q: parse private key: %w", name, err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if hasPwd {
		// Order matters. QNAP and Synology Dropbear/Mikrotik SSH builds
		// advertise keyboard-interactive as their primary password method;
		// when ssh.Password is tried first these servers respond to the
		// failed password packet with USERAUTH_FAILURE while the client is
		// still mid-state, manifesting as "unexpected message type 51
		// (expected 60)" instead of cleanly falling through to the next
		// method. Putting keyboard-interactive first matches what those
		// servers actually want and keeps classic ssh.Password as a
		// fallback for plain openssh servers that don't speak interactive.
		auths = append(auths, ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
			answers := make([]string, len(questions))
			for i := range answers {
				answers[i] = password
			}
			return answers, nil
		}))
		auths = append(auths, ssh.Password(password))
	}
	return auths, nil
}

func (s *sftpStorage) Name() string { return s.name }

// buildHostKeyCallback returns a strict known_hosts-backed callback when the
// destination Secret supplies a known-hosts blob. When it doesn't, the
// behaviour depends on allowInsecure: if true, falls back to
// InsecureIgnoreHostKey and logs a warning; if false, returns an error so
// misconfigured destinations fail loudly rather than silently accepting any
// host key.
//
// The known-hosts data is a standard ssh-keyscan-style file:
//
//	[hostname]:port ssh-ed25519 AAAAC3...
//	hostname,1.2.3.4 ssh-rsa AAAAB3...
//
// knownhosts.New only takes file paths, so we materialise the blob into a
// temp file just long enough to parse it; the resulting callback keeps the
// hosts table in memory and the file is removed before this function returns.
func buildHostKeyCallback(name string, knownHostsData []byte, allowInsecure bool, logger logr.Logger) (ssh.HostKeyCallback, error) {
	if len(knownHostsData) == 0 {
		if !allowInsecure {
			return nil, fmt.Errorf("no known-hosts supplied and insecure-skip-host-verify is not enabled; " +
				"provide known-hosts data or set insecure-skip-host-verify: \"true\" in the destination Secret")
		}
		logger.Info("INSECURE: no known-hosts supplied; accepting any host key for SFTP destination", "storage", name)
		return ssh.InsecureIgnoreHostKey(), nil
	}

	f, err := os.CreateTemp("", "backup-sftp-known-hosts-*")
	if err != nil {
		return nil, fmt.Errorf("known-hosts temp file: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()

	if _, err := f.Write(knownHostsData); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("known-hosts write: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("known-hosts close: %w", err)
	}

	cb, err := knownhosts.New(f.Name())
	if err != nil {
		return nil, fmt.Errorf("known-hosts parse: %w", err)
	}
	return cb, nil
}

func (s *sftpStorage) dial(ctx context.Context) (*ssh.Client, *sftp.Client, error) {
	cfg := &ssh.ClientConfig{
		User:            s.user,
		Auth:            s.auths,
		HostKeyCallback: s.hostKeyCB,
		Timeout:         30 * time.Second,
	}
	d := &net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return nil, nil, fmt.Errorf("dial %s: %w", s.addr, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, s.addr, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("ssh handshake: %w", err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)

	// Send keep-alive requests every 30 seconds so long-running sessions
	// (retention with many deletes) are not killed by idle-timeout on the
	// server side.
	//
	// The goroutine's lifetime is tied to the connection via client.Wait(),
	// which unblocks the instant the connection closes — caller's ssh.Close(),
	// a server-side drop, or pool eviction. Watching ctx.Done() alone was not
	// enough: a pooled client (dialed under the operator's long-lived context)
	// stays open across ticks, so when the pool finally closes it the keepalive
	// goroutine would survive until the next 30s tick happened to fail
	// SendRequest — leaking one goroutine per dial for up to a full interval.
	closed := make(chan struct{})
	go func() {
		_ = client.Wait()
		close(closed)
	}()
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
					return
				}
			case <-closed:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("sftp session: %w", err)
	}
	return client, sftpClient, nil
}

func (s *sftpStorage) full(p string) string {
	if s.pathPrefix == "" {
		return p
	}
	return path.Join(s.pathPrefix, p)
}

// stripPrefix turns a full server path back into the logical (prefix-less)
// path callers passed to Upload — so Object.Path round-trips through Get/Delete.
func (s *sftpStorage) stripPrefix(full string) string {
	if s.pathPrefix == "" {
		return full
	}
	rel := strings.TrimPrefix(full, s.pathPrefix)
	return strings.TrimPrefix(rel, "/")
}

func (s *sftpStorage) Upload(ctx context.Context, p string, r io.Reader) error {
	ssh, sc, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = ssh.Close() }()
	defer func() { _ = sc.Close() }()

	full := s.full(p)
	if err := sc.MkdirAll(path.Dir(full)); err != nil {
		return fmt.Errorf("mkdir %s: %w", path.Dir(full), err)
	}
	f, err := sc.Create(full)
	if err != nil {
		return fmt.Errorf("create %s: %w", full, err)
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		_ = sc.Remove(full) // remove partial file
		return fmt.Errorf("write %s: %w", full, err)
	}
	return f.Close()
}

// walkList traverses the directory tree and collects non-directory objects,
// checking for context cancellation between steps so long walks can be
// interrupted.
func walkList(ctx context.Context, sc *sftp.Client, root string, strip func(string) string) ([]storage.Object, error) {
	walker := sc.Walk(root)
	var out []storage.Object
	first := true
	for walker.Step() {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("walk cancelled: %w", err)
		}
		if err := walker.Err(); err != nil {
			// The root is lstat'd on the first step. A brand-new backup
			// target's directory does not exist until its first upload, so a
			// missing root is a legitimate empty listing, not a failure —
			// matching S3/Azure/GCS (empty prefix) and the FTPS root-550
			// case. Reporting an error here made the metrics refresher set
			// destination_failed=1 (false BackupDestinationFailing), the
			// first run's baseline read log "every destination failed", and
			// the pre-upload retention sweep emit a spurious RetentionFailed
			// event. Below the root, ErrNotExist means a concurrent delete
			// and is surfaced like any other walk error.
			if first && errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, fmt.Errorf("walk: %w", err)
		}
		first = false
		info := walker.Stat()
		if info.IsDir() {
			continue
		}
		out = append(out, storage.Object{
			Path:         strip(walker.Path()),
			Size:         info.Size(),
			LastModified: info.ModTime(),
		})
	}
	return out, nil
}

func (s *sftpStorage) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	ssh, sc, err := s.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ssh.Close() }()
	defer func() { _ = sc.Close() }()

	return walkList(ctx, sc, s.full(prefix), s.stripPrefix)
}

func (s *sftpStorage) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	ssh, sc, err := s.dial(ctx)
	if err != nil {
		return nil, err
	}
	f, err := sc.Open(s.full(p))
	if err != nil {
		_ = sc.Close()
		_ = ssh.Close()
		return nil, fmt.Errorf("open %s: %w", p, err)
	}
	return &sftpReader{f: f, sc: sc, ssh: ssh}, nil
}

type sftpReader struct {
	f   *sftp.File
	sc  *sftp.Client
	ssh *ssh.Client
}

func (r *sftpReader) Read(p []byte) (int, error) { return r.f.Read(p) }
func (r *sftpReader) Close() error {
	_ = r.f.Close()
	_ = r.sc.Close()
	return r.ssh.Close()
}

func (s *sftpStorage) Delete(ctx context.Context, p string) error {
	ssh, sc, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = ssh.Close() }()
	defer func() { _ = sc.Close() }()
	if err := sc.Remove(s.full(p)); err != nil {
		return fmt.Errorf("remove %s: %w", p, err)
	}
	return nil
}

// RemoveDirectory attempts to remove an empty directory. Returns an error if
// the directory is not empty or does not exist — callers should ignore errors.
func (s *sftpStorage) RemoveDirectory(ctx context.Context, p string) error {
	ssh, sc, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = ssh.Close() }()
	defer func() { _ = sc.Close() }()
	return sc.RemoveDirectory(s.full(p))
}

// WithSession opens one SSH+SFTP connection and returns a Storage that
// reuses it for every call. The caller MUST call closer() when done.
func (s *sftpStorage) WithSession(ctx context.Context) (storage.Storage, func() error, error) {
	sshC, sc, err := s.dial(ctx)
	if err != nil {
		return nil, nil, err
	}
	sess := &sftpSession{parent: s, sc: sc}
	closer := func() error {
		_ = sc.Close()
		return sshC.Close()
	}
	return sess, closer, nil
}

// sftpSession wraps a single SSH/SFTP connection so multiple List/Delete
// calls don't re-dial. Upload and Get still work but share the connection.
type sftpSession struct {
	parent *sftpStorage
	sc     *sftp.Client
}

func (s *sftpSession) Name() string { return s.parent.name }

func (s *sftpSession) Upload(_ context.Context, p string, r io.Reader) error {
	full := s.parent.full(p)
	if err := s.sc.MkdirAll(path.Dir(full)); err != nil {
		return fmt.Errorf("mkdir %s: %w", path.Dir(full), err)
	}
	f, err := s.sc.Create(full)
	if err != nil {
		return fmt.Errorf("create %s: %w", full, err)
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		_ = s.sc.Remove(full) // remove partial file
		return fmt.Errorf("write %s: %w", full, err)
	}
	return f.Close()
}

func (s *sftpSession) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	return walkList(ctx, s.sc, s.parent.full(prefix), s.parent.stripPrefix)
}

func (s *sftpSession) Get(_ context.Context, p string) (io.ReadCloser, error) {
	f, err := s.sc.Open(s.parent.full(p))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", p, err)
	}
	return f, nil
}

func (s *sftpSession) Delete(_ context.Context, p string) error {
	if err := s.sc.Remove(s.parent.full(p)); err != nil {
		return fmt.Errorf("remove %s: %w", p, err)
	}
	return nil
}

func (s *sftpSession) RemoveDirectory(_ context.Context, p string) error {
	return s.sc.RemoveDirectory(s.parent.full(p))
}
