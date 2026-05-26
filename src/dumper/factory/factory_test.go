package factory

import (
	"strings"
	"testing"

	"backup-operator/dumper"

	"github.com/go-logr/logr"
)

func TestNewDumper_AllTypes(t *testing.T) {
	cfg := dumper.Config{
		Host:     "localhost",
		Port:     5432,
		Database: "test",
		Username: "user",
		Password: "pass",
	}
	cases := []struct {
		dbType   string
		wantType string
	}{
		{TypePostgres, "postgres"},
		{TypeMySQL, "mysql"},
		{TypeMariaDB, "mariadb"},
		{TypeMongo, "mongo"},
		{TypeRedis, "redis"},
	}
	for _, c := range cases {
		d, err := NewDumper(c.dbType, cfg, logr.Discard())
		if err != nil {
			t.Errorf("NewDumper(%q) failed: %v", c.dbType, err)
			continue
		}
		if got := d.Type(); got != c.wantType {
			t.Errorf("NewDumper(%q).Type() = %q, want %q", c.dbType, got, c.wantType)
		}
	}
}

func TestNewDumper_Unsupported(t *testing.T) {
	_, err := NewDumper("sqlite", dumper.Config{}, logr.Discard())
	if err == nil {
		t.Fatal("expected error for unsupported db-type")
	}
	if !strings.Contains(err.Error(), "sqlite") {
		t.Errorf("error should mention the type, got: %v", err)
	}
}
