package mongo

import (
	"os"
	"strings"
	"testing"

	"backup-operator/dumper"

	"github.com/go-logr/logr"
)

func TestType(t *testing.T) {
	d := New(dumper.Config{}, logr.Discard())
	if got := d.Type(); got != "mongo" {
		t.Errorf("Type() = %q, want %q", got, "mongo")
	}
}

func TestBuildURI_WithoutCreds(t *testing.T) {
	d := &mongoDumper{cfg: dumper.Config{
		Host:     "mongo.prod.svc",
		Port:     27017,
		Username: "admin",
		Password: "s3cret",
	}}
	got := d.buildURI(false)
	if strings.Contains(got, "admin") {
		t.Errorf("buildURI(false) = %q, should not contain username", got)
	}
	if strings.Contains(got, "s3cret") {
		t.Errorf("buildURI(false) = %q, should not contain password", got)
	}
	if !strings.Contains(got, "mongodb://mongo.prod.svc:27017") {
		t.Errorf("buildURI(false) = %q, missing host:port", got)
	}
}

func TestBuildURI_WithCreds(t *testing.T) {
	d := &mongoDumper{cfg: dumper.Config{
		Host:     "mongo.prod.svc",
		Port:     27017,
		Username: "admin",
		Password: "s3cret",
	}}
	got := d.buildURI(true)
	if !strings.Contains(got, "admin:") {
		t.Errorf("buildURI(true) = %q, should contain username", got)
	}
}

func TestBuildURI_AuthSourceAndReplicaSet(t *testing.T) {
	d := &mongoDumper{cfg: dumper.Config{
		Host: "m",
		Port: 27017,
		Extra: map[string]string{
			"authSource": "admin",
			"replicaSet": "rs0",
		},
	}}
	got := d.buildURI(false)
	if !strings.Contains(got, "authSource=admin") {
		t.Errorf("buildURI() = %q, missing authSource", got)
	}
	if !strings.Contains(got, "replicaSet=rs0") {
		t.Errorf("buildURI() = %q, missing replicaSet", got)
	}
}

func TestBuildURI_EmptyExtras(t *testing.T) {
	d := &mongoDumper{cfg: dumper.Config{
		Host: "m",
		Port: 27017,
	}}
	got := d.buildURI(false)
	if strings.Contains(got, "authSource") || strings.Contains(got, "replicaSet") {
		t.Errorf("buildURI() = %q, should not contain extras when empty", got)
	}
}

func TestSystemDatabases(t *testing.T) {
	for _, db := range []string{"admin", "local", "config"} {
		if !systemDatabases[db] {
			t.Errorf("%q should be a system database", db)
		}
	}
	if systemDatabases["myapp"] {
		t.Error("myapp should not be a system database")
	}
}

func TestWriteMongoPasswordConfig(t *testing.T) {
	path, err := writeMongoPasswordConfig("hunter2")
	if err != nil {
		t.Fatalf("writeMongoPasswordConfig: %v", err)
	}
	defer func() { _ = os.Remove(path) }()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "password:") {
		t.Errorf("config should contain password key, got: %s", content)
	}
	if !strings.Contains(content, "hunter2") {
		t.Errorf("config should contain the password value, got: %s", content)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file permissions = %o, want 0600", perm)
	}
}

func TestWriteMongoPasswordConfig_SpecialChars(t *testing.T) {
	path, err := writeMongoPasswordConfig(`p@ss"word\with\special`)
	if err != nil {
		t.Fatalf("writeMongoPasswordConfig: %v", err)
	}
	defer func() { _ = os.Remove(path) }()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	content := string(data)
	// Backslashes and quotes should be escaped.
	if strings.Contains(content, `"word`) && !strings.Contains(content, `\"word`) {
		t.Errorf("unescaped quote in config: %s", content)
	}
}
