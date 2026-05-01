package mysql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"time"

	"backup-operator/dumper"

	"github.com/go-logr/logr"
	gomysql "github.com/go-sql-driver/mysql"
)

type mysqlDumper struct {
	cfg    dumper.Config
	logger logr.Logger
}

func New(cfg dumper.Config, logger logr.Logger) dumper.Dumper {
	return &mysqlDumper{cfg: cfg, logger: logger}
}

func (d *mysqlDumper) Type() string { return "mysql" }

func (d *mysqlDumper) Dump(ctx context.Context, w io.Writer) error {
	args := []string{
		"-h", d.cfg.Host,
		"-P", strconv.Itoa(d.cfg.Port),
		"-u", d.cfg.Username,
		fmt.Sprintf("-p%s", d.cfg.Password),
		"--single-transaction",
		"--quick",
		"--routines",
		"--triggers",
		d.cfg.Database,
	}
	cmd := exec.CommandContext(ctx, "mysqldump", args...)
	cmd.Stdout = w

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	d.logger.V(1).Info("running mysqldump", "host", d.cfg.Host, "db", d.cfg.Database)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mysqldump failed: %w: %s", err, stderr.String())
	}
	return nil
}

// CollectStats queries INFORMATION_SCHEMA. TABLE_ROWS is an estimate for
// InnoDB (the default engine) — accurate enough for anomaly detection and
// orders of magnitude cheaper than COUNT(*) on large tables.
func (d *mysqlDumper) CollectStats(ctx context.Context) (*dumper.Stats, error) {
	db, err := sql.Open("mysql", d.dsn())
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}

	tables, err := d.queryTables(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("query tables: %w", err)
	}

	hash, err := d.querySchemaHash(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("query schema: %w", err)
	}

	return &dumper.Stats{
		SchemaHash:  hash,
		Tables:      tables,
		GeneratedAt: time.Now().UTC(),
	}, nil
}

// dsn builds a go-sql-driver/mysql DSN. TLS defaults to "preferred" so that
// any TLS-capable server is used securely; opt out via extra-tls=false for
// purely internal links that don't support TLS at all.
func (d *mysqlDumper) dsn() string {
	cfg := gomysql.NewConfig()
	cfg.User = d.cfg.Username
	cfg.Passwd = d.cfg.Password
	cfg.Net = "tcp"
	cfg.Addr = d.cfg.Host + ":" + strconv.Itoa(d.cfg.Port)
	cfg.DBName = d.cfg.Database
	cfg.ParseTime = true

	tls := d.cfg.Extra["tls"]
	if tls == "" {
		tls = "preferred"
	}
	cfg.TLSConfig = tls

	return cfg.FormatDSN()
}

// scopeFilter returns the WHERE clause that excludes MySQL system schemas and
// optionally limits to the configured database. Returned args align with
// any '?' placeholders in the clause.
func (d *mysqlDumper) scopeFilter(prefix string) (string, []any) {
	clause := prefix + " TABLE_SCHEMA NOT IN ('mysql','information_schema','performance_schema','sys')"
	var args []any
	if d.cfg.Database != "" {
		clause += " AND TABLE_SCHEMA = ?"
		args = append(args, d.cfg.Database)
	}
	return clause, args
}

func (d *mysqlDumper) queryTables(ctx context.Context, db *sql.DB) ([]dumper.TableStats, error) {
	where, args := d.scopeFilter("WHERE TABLE_TYPE = 'BASE TABLE' AND")
	q := `
SELECT
  CONCAT(TABLE_SCHEMA, '.', TABLE_NAME)             AS name,
  COALESCE(TABLE_ROWS, 0)                            AS rows_est,
  COALESCE(DATA_LENGTH, 0) + COALESCE(INDEX_LENGTH, 0) AS size_bytes
FROM INFORMATION_SCHEMA.TABLES
` + where + `
ORDER BY name`

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

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

func (d *mysqlDumper) querySchemaHash(ctx context.Context, db *sql.DB) (string, error) {
	where, args := d.scopeFilter("WHERE")
	q := `
SELECT TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME, COLUMN_TYPE,
       IS_NULLABLE,
       COALESCE(COLUMN_DEFAULT, ''),
       COALESCE(COLUMN_KEY, '')
FROM INFORMATION_SCHEMA.COLUMNS
` + where + `
ORDER BY TABLE_SCHEMA, TABLE_NAME, ORDINAL_POSITION`

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()

	h := sha256.New()
	for rows.Next() {
		var schema, table, column, colType, nullable, def, key string
		if err := rows.Scan(&schema, &table, &column, &colType, &nullable, &def, &key); err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(h, "%s|%s|%s|%s|%s|%s|%s\n",
			schema, table, column, colType, nullable, def, key)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
