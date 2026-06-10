package restore

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/go-logr/logr"
	_ "github.com/go-sql-driver/mysql"

	"backup-operator/verifier/ephemeral"

	corev1 "k8s.io/api/core/v1"
)

const (
	mysqlVerifierUser = "root"
	mysqlVerifierDB   = "verify"
)

type mysqlEngine struct {
	dbType string // "mysql" or "mariadb"
}

func (e *mysqlEngine) DBType() string { return e.dbType }

func (e *mysqlEngine) PodSpec(volumeBytes int64, imageOverride string) ephemeral.Spec {
	image := imageOverride
	if image == "" {
		image = DefaultImage(e.dbType)
	}
	return ephemeral.Spec{
		Image: image,
		Port:  3306,
		EnvVars: []corev1.EnvVar{
			{Name: "MYSQL_ROOT_PASSWORD", Value: verifierPassword},
			{Name: "MYSQL_DATABASE", Value: mysqlVerifierDB},
		},
		VolumeMountPath: "/var/lib/mysql",
		VolumeSizeBytes: volumeBytes,
		ReadyTimeout:    5 * time.Minute,
		Probe:           probeMySQL,
	}
}

func probeMySQL(ctx context.Context, endpoint string) error {
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/%s?timeout=3s",
		mysqlVerifierUser, verifierPassword, endpoint, mysqlVerifierDB)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return db.PingContext(ctx)
}

func (e *mysqlEngine) Restore(ctx context.Context, endpoint string, plaintext io.Reader, mode Mode, log logr.Logger) error {
	host, port, err := splitEndpoint(endpoint)
	if err != nil {
		return err
	}

	in := plaintext
	if mode == ModeSchemaOnly {
		in = filterMySQLSchemaOnly(plaintext)
	}

	// MYSQL_PWD over -p<password> so the password never lives on the
	// command line (matches the existing dumper-side hardening).
	cmd := exec.CommandContext(ctx, "mysql",
		"-h", host,
		"-P", port,
		"-u", mysqlVerifierUser,
		"--default-character-set=utf8mb4",
		mysqlVerifierDB,
	)
	cmd.Env = append(cmd.Env, "MYSQL_PWD="+verifierPassword, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	cmd.Stdin = in
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	log.V(1).Info("running mysql restore", "endpoint", endpoint, "mode", mode)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mysql restore: %w; stderr: %s", err, sanitiseStderr(stderr.String()))
	}
	return nil
}

func (e *mysqlEngine) SmokeQueries(ctx context.Context, endpoint string, preTables map[string]int64, mode Mode, log logr.Logger) (*SmokeResult, error) {
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/%s?timeout=10s",
		mysqlVerifierUser, verifierPassword, endpoint, mysqlVerifierDB)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("smoke open: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("smoke ping: %w", err)
	}

	res := &SmokeResult{}
	for tableName, expected := range preTables {
		// Pre-stats names from CollectStats are sometimes "schema.table"
		// (postgres-style) and sometimes plain "table" (mysql-style).
		// mysqldump emits unqualified names, but CollectStats is
		// schema-aware. Try both: the qualified name first, then the
		// unqualified tail if that fails.
		got, err := mysqlCount(ctx, db, tableName)
		if err != nil {
			if dot := strings.LastIndexByte(tableName, '.'); dot > 0 {
				short := tableName[dot+1:]
				if g2, e2 := mysqlCount(ctx, db, short); e2 == nil {
					got, err = g2, nil
				}
			}
		}
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

func mysqlCount(ctx context.Context, db *sql.DB, tableName string) (int64, error) {
	var got int64
	q := fmt.Sprintf("SELECT count(*) FROM %s", quoteMySQLIdent(tableName))
	err := db.QueryRowContext(ctx, q).Scan(&got)
	return got, err
}

func quoteMySQLIdent(name string) string {
	parts := strings.Split(name, ".")
	for i, p := range parts {
		parts[i] = "`" + strings.ReplaceAll(p, "`", "``") + "`"
	}
	return strings.Join(parts, ".")
}

// filterMySQLSchemaOnly drops INSERT INTO statements but preserves CREATE
// TABLE, ALTER, DROP, USE, SET. mysqldump emits each INSERT on its own
// line (or extended-insert as one long line), and other statements never
// start with "INSERT INTO" — so a line-prefix filter is correct.
func filterMySQLSchemaOnly(r io.Reader) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		s := bufio.NewScanner(r)
		s.Buffer(make([]byte, 256*1024), 32*1024*1024)
		for s.Scan() {
			line := s.Text()
			if strings.HasPrefix(line, "INSERT INTO ") {
				continue
			}
			if _, err := pw.Write([]byte(line + "\n")); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		// Propagate a scan failure (bufio.ErrTooLong on a >32 MB extended-
		// insert line, or a truncated/corrupt decrypted stream that never
		// yields a newline) to the mysql client as an error rather than a
		// clean EOF — otherwise it exits 0 and a truncated dump verifies as
		// "match". CloseWithError(nil) is equivalent to Close() (signals EOF).
		pw.CloseWithError(s.Err())
	}()
	return pr
}
