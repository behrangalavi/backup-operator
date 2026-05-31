package mysql

import (
	"strings"
	"testing"

	"backup-operator/dumper"

	"github.com/go-logr/logr"
)

func TestType_MySQL(t *testing.T) {
	d := New("mysql", dumper.Config{}, logr.Discard())
	if got := d.Type(); got != "mysql" {
		t.Errorf("Type() = %q, want %q", got, "mysql")
	}
}

func TestType_MariaDB(t *testing.T) {
	d := New("mariadb", dumper.Config{}, logr.Discard())
	if got := d.Type(); got != "mariadb" {
		t.Errorf("Type() = %q, want %q", got, "mariadb")
	}
}

func TestType_EmptyDefaultsMySQL(t *testing.T) {
	d := New("", dumper.Config{}, logr.Discard())
	if got := d.Type(); got != "mysql" {
		t.Errorf("Type() with empty dbType = %q, want %q", got, "mysql")
	}
}

func TestDumpBinary(t *testing.T) {
	d := &mysqlDumper{dbType: "mysql"}
	if got := d.dumpBinary(); got != "mysqldump" {
		t.Errorf("dumpBinary() = %q, want %q", got, "mysqldump")
	}
}

func TestDSN(t *testing.T) {
	d := &mysqlDumper{cfg: dumper.Config{
		Host:     "db.internal",
		Port:     3306,
		Username: "root",
		Password: "secret",
		Database: "myapp",
	}}
	got := d.dsn()
	for _, want := range []string{"root:", "@tcp(db.internal:3306)", "/myapp"} {
		if !strings.Contains(got, want) {
			t.Errorf("dsn() = %q, missing %q", got, want)
		}
	}
}

func TestDSN_TLSDefault(t *testing.T) {
	d := &mysqlDumper{cfg: dumper.Config{
		Host:     "h",
		Port:     3306,
		Username: "u",
		Password: "p",
		Database: "d",
	}}
	got := d.dsn()
	if !strings.Contains(got, "tls=preferred") {
		t.Errorf("dsn() = %q, expected tls=preferred as default", got)
	}
}

func TestDSN_TLSOverride(t *testing.T) {
	d := &mysqlDumper{cfg: dumper.Config{
		Host:     "h",
		Port:     3306,
		Username: "u",
		Password: "p",
		Database: "d",
		Extra:    map[string]string{"tls": "skip-verify"},
	}}
	got := d.dsn()
	if !strings.Contains(got, "tls=skip-verify") {
		t.Errorf("dsn() = %q, expected tls=skip-verify", got)
	}
}

func argList(args []string) string { return strings.Join(args, " ") }

func TestBuildDumpArgs_Defaults(t *testing.T) {
	d := &mysqlDumper{cfg: dumper.Config{
		Host: "h", Port: 3306, Username: "u", Database: "mydb",
	}}
	args := d.buildDumpArgs("mariadb-dump") // mariadb-dump: no column-statistics probe interference
	joined := argList(args)
	for _, want := range []string{
		"--single-transaction", "--quick", "--routines", "--triggers",
		"--events", "--default-character-set=utf8mb4",
		"--max-allowed-packet=1G",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("buildDumpArgs() missing %q in: %s", want, joined)
		}
	}
	// Database must be the final positional argument.
	if args[len(args)-1] != "mydb" {
		t.Errorf("expected database as last arg, got %q (full: %s)", args[len(args)-1], joined)
	}
}

func TestBuildDumpArgs_MaxAllowedPacketOverride(t *testing.T) {
	d := &mysqlDumper{cfg: dumper.Config{
		Host: "h", Port: 3306, Username: "u", Database: "mydb",
		Extra: map[string]string{"max-allowed-packet": "512M"},
	}}
	joined := argList(d.buildDumpArgs("mariadb-dump"))
	if !strings.Contains(joined, "--max-allowed-packet=512M") {
		t.Errorf("override not applied: %s", joined)
	}
	if strings.Contains(joined, "--max-allowed-packet=1G") {
		t.Errorf("default should not appear alongside override: %s", joined)
	}
}

func TestScopeFilter_WithDatabase(t *testing.T) {
	d := &mysqlDumper{cfg: dumper.Config{Database: "mydb"}}
	clause, args := d.scopeFilter("WHERE")
	if !strings.Contains(clause, "TABLE_SCHEMA NOT IN") {
		t.Errorf("scopeFilter clause missing system schema exclusion: %s", clause)
	}
	if !strings.Contains(clause, "TABLE_SCHEMA = ?") {
		t.Errorf("scopeFilter clause missing database filter: %s", clause)
	}
	if len(args) != 1 || args[0] != "mydb" {
		t.Errorf("scopeFilter args = %v, want [mydb]", args)
	}
}

func TestScopeFilter_NoDatabase(t *testing.T) {
	d := &mysqlDumper{cfg: dumper.Config{Database: ""}}
	clause, args := d.scopeFilter("WHERE")
	if strings.Contains(clause, "TABLE_SCHEMA = ?") {
		t.Errorf("scopeFilter with empty DB should not include TABLE_SCHEMA = ?: %s", clause)
	}
	if len(args) != 0 {
		t.Errorf("scopeFilter args should be empty, got %v", args)
	}
}

func TestScopeFilter_CustomPrefix(t *testing.T) {
	d := &mysqlDumper{cfg: dumper.Config{Database: "x"}}
	clause, _ := d.scopeFilter("AND")
	if !strings.HasPrefix(clause, "AND ") {
		t.Errorf("scopeFilter should use provided prefix, got: %s", clause)
	}
}
