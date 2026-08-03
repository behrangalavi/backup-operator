// Package restore drives the Phase-2 verifier modes: schema-only,
// sample, and full. Each spawns an ephemeral DB pod via the Spawner,
// pipes the decrypted dump stream into the engine's restore tool, and
// runs smoke queries against the restored database.
//
// The restore engine knows three things about its DB type:
//   1. How to spawn a fresh empty DB (image, env, port, ready-probe)
//   2. How to restore the dump stream into that DB
//   3. How to ask the DB "did the data land?" via smoke queries
//
// These split nicely along an interface so the verifier-driver can be
// engine-agnostic, and the per-engine details are testable in isolation.
package restore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"backup-operator/verifier/ephemeral"

	"github.com/go-logr/logr"
)

// Mode is the restore-mode the verifier asked for. The engine uses it
// to pick (a) the restore command flags (e.g. pg_restore --schema-only
// vs full) and (b) the smoke-query depth.
type Mode string

const (
	ModeSchemaOnly Mode = "schema-only"
	ModeSample     Mode = "sample"
	ModeFull       Mode = "full"
)

// SmokeResult is the outcome of running smoke queries against the
// restored DB. TableRows compares what landed against expected pre-stats
// (caller pre-fills Expected); empty TableRows means smoke queries don't
// surface row counts (e.g. mongo / redis schema-only).
type SmokeResult struct {
	Tables []TableSmoke
	Notes  []string
}

// TableSmoke is one row of the post-restore consistency check.
type TableSmoke struct {
	Name     string
	Expected int64
	Got      int64
	Match    bool
}

// Engine is the per-DB-type restore driver.
type Engine interface {
	// DBType returns the canonical type label (postgres, mysql, mariadb,
	// mongo, redis). Used for routing and logging.
	DBType() string

	// PodSpec returns the ephemeral.Spec the spawner needs for THIS
	// engine. NamePrefix and OwnerRef are filled in by the verifier
	// driver before passing to Spawn(); engine fills the rest.
	PodSpec(volumeBytes int64, imageOverride string) ephemeral.Spec

	// Restore feeds the decrypted plaintext dump stream into the
	// running ephemeral DB at endpoint. mode controls schema-only vs
	// full vs sample (sample is currently equivalent to full from the
	// engine's perspective — pre-filtering happens upstream in Phase 3).
	Restore(ctx context.Context, endpoint string, plaintext io.Reader, mode Mode, log logr.Logger) error

	// SmokeQueries runs the post-restore consistency check. preTables
	// is the pre-dump per-table row counts; the engine compares against
	// what its now sees in the freshly-restored DB.
	SmokeQueries(ctx context.Context, endpoint string, preTables map[string]int64, mode Mode, log logr.Logger) (*SmokeResult, error)
}

// NewEngine returns the engine for the given dbType, or an error for
// unsupported types. Unknown types are NOT silently skipped — that
// would mask a misconfigured source.
func NewEngine(dbType string) (Engine, error) {
	// Each engine gets a fresh random password, so the ephemeral verifier DB
	// pod is never reachable with a compile-time-known credential. A leaked or
	// observed password then grants access to a single short-lived pod holding
	// one run's data — not the fleet.
	pw, err := randomPassword()
	if err != nil {
		return nil, fmt.Errorf("restore engine: generate ephemeral password: %w", err)
	}
	switch dbType {
	case "postgres":
		return &postgresEngine{password: pw}, nil
	case "mysql", "mariadb":
		return &mysqlEngine{dbType: dbType, password: pw}, nil
	case "mongo":
		return &mongoEngine{password: pw}, nil
	case "redis":
		return &redisEngine{password: pw}, nil
	}
	return nil, fmt.Errorf("restore engine: unsupported db-type %q", dbType)
}

// randomPassword returns a 32-hex-char (16-byte) credential for the ephemeral
// verifier DB pod. Hex avoids any character that would need escaping in a DSN,
// URI, or CLI argument across the four engines.
func randomPassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// DefaultImage returns the default container image for an engine when
// the source has no `verification-image` annotation. Tags are pinned to
// recent stable major versions; users with version-sensitive restores
// SHOULD set the annotation explicitly.
func DefaultImage(dbType string) string {
	switch dbType {
	case "postgres":
		return "postgres:16-alpine"
	case "mysql":
		return "mysql:8.0"
	case "mariadb":
		return "mariadb:11"
	case "mongo":
		return "mongo:7"
	case "redis":
		return "redis:7-alpine"
	}
	return ""
}

// runAsUIDForImage returns the UID the non-root DB user is baked in as for the
// (possibly overridden) image. The container must run as exactly this UID under
// runAsNonRoot, or the entrypoint fails "could not look up effective user ID"
// and the pod never becomes ready. Only postgres differs by variant: the alpine
// image uses UID 70, the Debian image 999 — detected from the tag so a
// verification-image override to a Debian postgres still gets the right UID.
// mysql/mariadb/mongo (Debian) and redis (alpine) all use 999.
func runAsUIDForImage(dbType, image string) int64 {
	if dbType == "postgres" && strings.Contains(image, "alpine") {
		return 70
	}
	return 999
}
