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
	"fmt"
	"io"

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
	switch dbType {
	case "postgres":
		return &postgresEngine{}, nil
	case "mysql", "mariadb":
		return &mysqlEngine{dbType: dbType}, nil
	case "mongo":
		return &mongoEngine{}, nil
	case "redis":
		return &redisEngine{}, nil
	}
	return nil, fmt.Errorf("restore engine: unsupported db-type %q", dbType)
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
