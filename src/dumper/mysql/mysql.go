package mysql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"backup-operator/dumper"

	"github.com/go-logr/logr"
	gomysql "github.com/go-sql-driver/mysql"
)

// supportsColumnStatistics reports whether the given dump binary
// recognises `--column-statistics`. Oracle's MySQL-8 `mysqldump` does
// (and uses it to silence its column_statistics probe against older
// MySQL / MariaDB targets); `mariadb-dump` does not and aborts with
// "unknown variable 'column-statistics=0'" when given the flag.
//
// Detection runs `<binary> --version` once per binary path per worker
// process and looks for the literal "MariaDB" — present in MariaDB's
// version banner ("Distrib 10.11.x-MariaDB"), absent in Oracle's. We
// invert from there: anything that does not self-identify as MariaDB
// is treated as Oracle and gets the flag. Probing --version (vs --help)
// is robust against future MariaDB versions adding compatibility
// mentions of "column-statistics" to their help text.
//
// Probe failure (binary missing, exec error) returns false — the dump
// call that follows will surface the real error if the tool is
// unreachable. Result is cached per binary path; the worker is
// one-shot so this is "once per backup run".
var binaryProbeCache sync.Map // key: binary path, value: bool

func supportsColumnStatistics(binary string) bool {
	if v, ok := binaryProbeCache.Load(binary); ok {
		return v.(bool)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "--version").CombinedOutput()
	supports := false
	if err == nil && !strings.Contains(string(out), "MariaDB") {
		supports = true
	}
	binaryProbeCache.Store(binary, supports)
	return supports
}

type mysqlDumper struct {
	cfg    dumper.Config
	dbType string // "mysql" or "mariadb" — picks the dump binary
	logger logr.Logger
}

// New returns a dumper that talks the MySQL wire protocol. dbType picks
// the dump binary: "mysql" → Oracle's `mysqldump` (mysql-community-client),
// "mariadb" → `mariadb-dump`. For backwards-compat, an empty dbType
// defaults to "mysql".
func New(dbType string, cfg dumper.Config, logger logr.Logger) dumper.Dumper {
	if dbType == "" {
		dbType = "mysql"
	}
	return &mysqlDumper{cfg: cfg, dbType: dbType, logger: logger}
}

func (d *mysqlDumper) Type() string { return d.dbType }

// dumpBinary returns the dump tool name. The worker image ships
// exactly one mysql-protocol dump binary per arch (mysql-community-
// client-core on amd64, mariadb-client on arm64) because both
// packages claim virtual-mysql-client-core and refuse to coexist.
// Both speak the MySQL wire protocol, so a single binary handles
// both `dbType=mysql` and `dbType=mariadb` sources. The probe-once
// logic on `--column-statistics` keeps the flag correct regardless
// of which binary is present.
func (d *mysqlDumper) dumpBinary() string {
	return "mysqldump"
}

func (d *mysqlDumper) Dump(ctx context.Context, w io.Writer) error {
	// Flag rationale:
	//   --single-transaction: InnoDB-consistent snapshot without locking.
	//   --quick: stream rows without buffering — required for large tables.
	//   --routines / --triggers / --events: include stored procs, triggers,
	//     and the MySQL Event Scheduler. Without these, restore loses
	//     server-side logic the application may depend on.
	//   --default-character-set=utf8mb4: emit dump in utf8mb4 so umlauts
	//     and emoji round-trip correctly. The pre-utf8mb4 default ("utf8"
	//     in MySQL <8) is actually 3-byte and silently truncates 4-byte
	//     characters at restore time.
	//   --column-statistics=0: Oracle's MySQL-8 mysqldump probes
	//     information_schema.column_statistics by default and fails
	//     when the target lacks the table (older MySQL, MariaDB).
	//     Setting =0 disables the probe — Oracle's documented compat
	//     flag. mariadb-dump does not know the flag and aborts with
	//     "unknown variable" if given it. supportsColumnStatistics()
	//     probes `<binary> --help` once per binary so we pass the flag
	//     only when it's actually accepted.
	args := []string{
		"-h", d.cfg.Host,
		"-P", strconv.Itoa(d.cfg.Port),
		"-u", d.cfg.Username,
		"--single-transaction",
		"--quick",
		"--routines",
		"--triggers",
		"--events",
		"--default-character-set=utf8mb4",
		d.cfg.Database,
	}
	bin := d.dumpBinary()
	if supportsColumnStatistics(bin) {
		// Insert before the trailing database arg.
		args = append(args[:len(args)-1], append([]string{"--column-statistics=0"}, args[len(args)-1])...)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	// Pass the password via MYSQL_PWD instead of `-p<value>` on the command
	// line. `-p` would be visible in `ps` output and any process-listing
	// telemetry; the env var stays inside the worker pod.
	if d.cfg.Password != "" {
		cmd.Env = append(os.Environ(), "MYSQL_PWD="+d.cfg.Password)
	}
	cmd.Stdout = w

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	d.logger.V(1).Info("running dump tool", "binary", bin, "host", d.cfg.Host, "db", d.cfg.Database)
	if err := cmd.Run(); err != nil {
		return dumper.WrapExecError(bin, err, stderr.String(), d.cfg.Password)
	}
	return nil
}

// CollectStats queries INFORMATION_SCHEMA. TABLE_ROWS is an estimate for
// InnoDB (the default engine) — accurate enough for anomaly detection and
// orders of magnitude cheaper than COUNT(*) on large tables.
func (d *mysqlDumper) CollectStats(ctx context.Context) (*dumper.Stats, error) {
	db, err := sql.Open("mysql", d.dsn())
	if err != nil {
		return nil, dumper.SanitizeError("open", err, d.cfg.Password)
	}
	defer func() { _ = db.Close() }()

	if err := db.PingContext(ctx); err != nil {
		return nil, dumper.SanitizeError("ping", err, d.cfg.Password)
	}

	tables, err := d.queryTables(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("query tables: %w", err)
	}

	hash, err := d.querySchemaHash(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("query schema: %w", err)
	}

	// Best-effort encoding capture: drift between runs flags a server upgrade
	// or migration that may silently break utf8 → utf8mb4 on restore.
	charset, collation := d.queryEncoding(ctx, db)

	return &dumper.Stats{
		SchemaHash:  hash,
		Charset:     charset,
		Collation:   collation,
		Tables:      tables,
		GeneratedAt: time.Now().UTC(),
	}, nil
}

func (d *mysqlDumper) queryEncoding(ctx context.Context, db *sql.DB) (string, string) {
	const q = `SELECT @@character_set_database, @@collation_database`
	var charset, collation string
	if err := db.QueryRowContext(ctx, q).Scan(&charset, &collation); err != nil {
		d.logger.V(1).Info("query encoding skipped", "err", err.Error())
		return "", ""
	}
	return charset, collation
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
