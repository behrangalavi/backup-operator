package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"time"

	"backup-operator/dumper"

	"github.com/go-logr/logr"
	"github.com/jackc/pgx/v5"
)

type postgresDumper struct {
	cfg    dumper.Config
	logger logr.Logger
}

func New(cfg dumper.Config, logger logr.Logger) dumper.Dumper {
	return &postgresDumper{cfg: cfg, logger: logger}
}

func (d *postgresDumper) Type() string { return "postgres" }

func (d *postgresDumper) Dump(ctx context.Context, w io.Writer) error {
	args := []string{
		"-h", d.cfg.Host,
		"-p", strconv.Itoa(d.cfg.Port),
		"-U", d.cfg.Username,
		"-d", d.cfg.Database,
		"--no-owner",
		"--no-privileges",
	}
	cmd := exec.CommandContext(ctx, "pg_dump", args...)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+d.cfg.Password)
	cmd.Stdout = w

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	d.logger.V(1).Info("running pg_dump", "host", d.cfg.Host, "db", d.cfg.Database)
	if err := cmd.Run(); err != nil {
		return dumper.WrapExecError("pg_dump", err, stderr.String(), d.cfg.Password)
	}
	return nil
}

// CollectStats opens a regular SQL connection and gathers per-table row count
// estimates plus a schema fingerprint. Estimates from pg_stat_user_tables are
// accurate enough for anomaly detection — exact COUNT(*) on every table would
// be cost-prohibitive on large databases.
func (d *postgresDumper) CollectStats(ctx context.Context) (*dumper.Stats, error) {
	conn, err := pgx.Connect(ctx, d.connString())
	if err != nil {
		return nil, dumper.SanitizeError("connect", err, d.cfg.Password)
	}
	defer func() { _ = conn.Close(ctx) }()

	tables, err := d.queryTables(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("query tables: %w", err)
	}

	hash, err := d.querySchemaHash(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("query schema: %w", err)
	}

	return &dumper.Stats{
		SchemaHash:  hash,
		Tables:      tables,
		GeneratedAt: time.Now().UTC(),
	}, nil
}

func (d *postgresDumper) connString() string {
	u := url.URL{
		Scheme: "postgres",
		Host:   d.cfg.Host + ":" + strconv.Itoa(d.cfg.Port),
		User:   url.UserPassword(d.cfg.Username, d.cfg.Password),
		Path:   "/" + d.cfg.Database,
	}
	q := u.Query()
	sslmode := d.cfg.Extra["sslmode"]
	if sslmode == "" {
		sslmode = "prefer"
	}
	q.Set("sslmode", sslmode)
	u.RawQuery = q.Encode()
	return u.String()
}

// queryTables uses pg_stat_user_tables for fast row estimates and
// pg_total_relation_size for on-disk size including indexes/toast.
func (d *postgresDumper) queryTables(ctx context.Context, conn *pgx.Conn) ([]dumper.TableStats, error) {
	const q = `
SELECT
  schemaname || '.' || relname AS name,
  COALESCE(n_live_tup, 0)      AS rows,
  pg_total_relation_size(relid) AS size_bytes
FROM pg_stat_user_tables
ORDER BY name`
	rows, err := conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []dumper.TableStats
	for rows.Next() {
		var t dumper.TableStats
		if err := rows.Scan(&t.Name, &t.RowCount, &t.SizeBytes); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// querySchemaHash computes a stable fingerprint over user-schema column
// definitions. Stable ordering is critical — any DDL change (added column,
// type change, dropped table) flips the hash.
func (d *postgresDumper) querySchemaHash(ctx context.Context, conn *pgx.Conn) (string, error) {
	const q = `
SELECT table_schema, table_name, column_name, data_type,
       COALESCE(character_maximum_length, -1),
       is_nullable,
       COALESCE(column_default, '')
FROM information_schema.columns
WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
ORDER BY table_schema, table_name, ordinal_position`
	rows, err := conn.Query(ctx, q)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	h := sha256.New()
	for rows.Next() {
		var (
			schema, table, column, dataType, isNullable, colDefault string
			maxLen                                                  int
		)
		if err := rows.Scan(&schema, &table, &column, &dataType, &maxLen, &isNullable, &colDefault); err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(h, "%s|%s|%s|%s|%d|%s|%s\n",
			schema, table, column, dataType, maxLen, isNullable, colDefault)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
