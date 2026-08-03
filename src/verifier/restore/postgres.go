package restore

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/jackc/pgx/v5"

	"backup-operator/verifier/ephemeral"

	corev1 "k8s.io/api/core/v1"
)

const (
	verifierUser = "postgres"
	verifierDB   = "verify"
)

type postgresEngine struct {
	// password is the per-run root credential for the ephemeral pod,
	// generated in NewEngine. Never a compile-time constant.
	password string
}

func (*postgresEngine) DBType() string { return "postgres" }

func (e *postgresEngine) PodSpec(volumeBytes int64, imageOverride string) ephemeral.Spec {
	image := imageOverride
	if image == "" {
		image = DefaultImage("postgres")
	}
	pw := e.password
	return ephemeral.Spec{
		Image: image,
		Port:  5432,
		EnvVars: []corev1.EnvVar{
			{Name: "POSTGRES_PASSWORD", Value: pw},
			{Name: "POSTGRES_DB", Value: verifierDB},
			// Postgres image refuses to run if PGDATA is on a mount with
			// existing files; pin it to a subdir of our emptyDir.
			{Name: "PGDATA", Value: "/data/pgdata"},
		},
		VolumeMountPath: "/data",
		VolumeSizeBytes: volumeBytes,
		ReadyTimeout:    5 * time.Minute,
		RunAsUID:        runAsUIDForImage("postgres", image),
		Probe: func(ctx context.Context, endpoint string) error {
			return probePostgres(ctx, endpoint, pw)
		},
	}
}

// probePostgres opens a pgx connection and runs SELECT 1. The default
// postgres image's healthcheck is shell-based and we can't rely on its
// readinessProbe being configured — so we own the readiness signal.
func probePostgres(ctx context.Context, endpoint, password string) error {
	dsn := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable&connect_timeout=3",
		verifierUser, password, endpoint, verifierDB)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	var n int
	return conn.QueryRow(ctx, "SELECT 1").Scan(&n)
}

func (e *postgresEngine) Restore(ctx context.Context, endpoint string, plaintext io.Reader, mode Mode, log logr.Logger) error {
	host, port, err := splitEndpoint(endpoint)
	if err != nil {
		return err
	}

	// schema-only mode: stream-filter COPY data blocks so postgres only
	// ever sees DDL. Lets the verifier prove "schema restore works"
	// without paying the data restore cost on a 50 GiB DB. For sample
	// and full, the unfiltered stream is fed straight in.
	in := plaintext
	if mode == ModeSchemaOnly {
		in = filterPostgresSchemaOnly(plaintext)
	}

	// psql --set ON_ERROR_STOP=on aborts on the first SQL error rather
	// than continuing through a half-restored DB and reporting success.
	cmd := exec.CommandContext(ctx, "psql",
		"-h", host,
		"-p", port,
		"-U", verifierUser,
		"-d", verifierDB,
		"--no-psqlrc",
		"--quiet",
		"--set", "ON_ERROR_STOP=on",
	)
	cmd.Env = append(cmd.Env, "PGPASSWORD="+e.password, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	cmd.Stdin = in
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	log.V(1).Info("running psql restore", "endpoint", endpoint, "mode", mode)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("psql restore: %w; stderr: %s", err, sanitiseStderr(stderr.String(), e.password))
	}
	return nil
}

func (e *postgresEngine) SmokeQueries(ctx context.Context, endpoint string, preTables map[string]int64, mode Mode, log logr.Logger) (*SmokeResult, error) {
	dsn := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
		verifierUser, e.password, endpoint, verifierDB)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("smoke connect: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	res := &SmokeResult{}
	for tableName, expected := range preTables {
		// pgx automatically quotes; we pass the schema-qualified name as
		// raw identifier via fmt — table names from the source's stats
		// are trusted (came from pg_stat_user_tables on a DB the operator
		// already has read access to). Still, no string-formatted SQL
		// for values; only for identifiers.
		query := fmt.Sprintf("SELECT count(*) FROM %s", quotePostgresIdent(tableName))
		var got int64
		err := conn.QueryRow(ctx, query).Scan(&got)
		if err != nil {
			res.Tables = append(res.Tables, TableSmoke{Name: tableName, Expected: expected, Got: -1, Match: false})
			res.Notes = append(res.Notes, fmt.Sprintf("query failed for %s: %v", tableName, err))
			continue
		}
		match := smokeMatch(mode, expected, got)
		res.Tables = append(res.Tables, TableSmoke{Name: tableName, Expected: expected, Got: got, Match: match})
	}
	return res, nil
}

// filterPostgresSchemaOnly returns a Reader that drops every COPY-data
// payload from a plain SQL pg_dump while keeping all DDL, COPY headers
// (so they syntactically validate), and the trailing `\.` terminator.
//
// Implemented as a goroutine-piped scanner so the underlying stream can
// still come from a network socket / age-decryptor without buffering the
// whole dump.
func filterPostgresSchemaOnly(r io.Reader) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		s := bufio.NewScanner(r)
		s.Buffer(make([]byte, 256*1024), 32*1024*1024)
		inCopy := false
		for s.Scan() {
			line := s.Text()
			if !inCopy && strings.HasPrefix(line, "COPY ") && strings.Contains(line, " FROM stdin;") {
				// Replace the body with an immediate `\.` so the COPY
				// statement parses but writes zero rows.
				if _, err := pw.Write([]byte(line + "\n\\.\n")); err != nil {
					pw.CloseWithError(err)
					return
				}
				inCopy = true
				continue
			}
			if inCopy {
				if line == `\.` {
					inCopy = false
				}
				continue
			}
			if _, err := pw.Write([]byte(line + "\n")); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		// Propagate a scan failure (bufio.ErrTooLong on a >32 MB line, or a
		// truncated/corrupt decrypted stream that never yields a newline) to
		// psql as an error rather than a clean EOF. Without this, psql reads
		// the partial DDL, exits 0, and a truncated dump verifies as "match".
		// CloseWithError(nil) is equivalent to Close() (signals EOF).
		pw.CloseWithError(s.Err())
	}()
	return pr
}

// quotePostgresIdent wraps a possibly-qualified identifier
// (schema.table) so each part is double-quoted and embedded quotes are
// doubled. Belt-and-braces — preStats names come from a trusted source.
func quotePostgresIdent(name string) string {
	parts := strings.Split(name, ".")
	for i, p := range parts {
		parts[i] = `"` + strings.ReplaceAll(p, `"`, `""`) + `"`
	}
	return strings.Join(parts, ".")
}

// smokeMatch returns whether a per-table restored count is acceptable for
// the active mode. schema-only just requires the table exists (count
// query succeeded); sample/full want the count to roughly equal the
// pre-stats baseline, with the same 99% tolerance the dump-time
// verifier uses.
func smokeMatch(mode Mode, expected, got int64) bool {
	if mode == ModeSchemaOnly {
		return got >= 0
	}
	if expected == 0 {
		return true
	}
	if got >= expected {
		return true
	}
	ratio := float64(got) / float64(expected)
	return ratio >= 0.99
}

// splitEndpoint parses "host:port".
func splitEndpoint(ep string) (string, string, error) {
	idx := strings.LastIndex(ep, ":")
	if idx <= 0 {
		return "", "", fmt.Errorf("invalid endpoint %q (want host:port)", ep)
	}
	return ep[:idx], ep[idx+1:], nil
}

// sanitiseStderr removes the per-run verifier password from CLI error output
// before it lands in logs / events. Not security-critical (the password is
// ephemeral) but prevents accidental capture in incident reports.
func sanitiseStderr(s, password string) string {
	if password == "" {
		return s
	}
	return strings.ReplaceAll(s, password, "***")
}
