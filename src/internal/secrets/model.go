// Package secrets turns the backup.mogenius.io API objects plus their
// credential Secrets into the flat Source / Destination structs the
// pipeline, controllers and UI consume. The name is historical — discovery
// used to be driven by labelled Secrets — and is kept because every
// consumer imports it; the types it exports are what matter.
package secrets

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"backup-operator/api/v1alpha1"
	"backup-operator/dumper"
)

// validName is the conservative allow-list for logical identifiers (source
// target name, destination name) that flow into object-storage paths, metric
// labels, and CronJob names. It forbids the path separator and any leading
// dot, so a value like "../other-tenant" or "a/b" cannot escape a
// destination's path-prefix once path.Join'd into an object key. Without a
// '/' a literal ".." is inert (path.Clean leaves it in place), so barring the
// separator is sufficient to close the traversal class.
//
// The CRD schema enforces the same pattern on spec.targetName at admission;
// this is the defence for callers that bypass the API server (tests, the
// migration tool) and for metadata.name, which Kubernetes validates with a
// looser DNS rule.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validateName rejects identifiers that could escape the storage prefix or
// break path/label construction.
func validateName(kind, v string) error {
	if len(v) > 253 {
		return fmt.Errorf("%s %q is too long (max 253 characters)", kind, v)
	}
	if !validName.MatchString(v) {
		return fmt.Errorf("%s %q is invalid: must match [A-Za-z0-9][A-Za-z0-9._-]* (no '/', no leading '.')", kind, v)
	}
	return nil
}

// validatePathPrefix allows the '/' separator (a prefix like "/cluster-prod"
// is the documented isolation root) but rejects any ".." segment, which would
// otherwise let one destination's prefix climb into another's namespace.
func validatePathPrefix(v string) error {
	for _, seg := range strings.Split(v, "/") {
		if seg == ".." {
			return fmt.Errorf("pathPrefix %q must not contain a %q segment", v, "..")
		}
	}
	return nil
}

// DefaultRestoreVerificationInterval is used when restoreVerification.mode is
// active but no interval is set. Weekly cadence balances signal freshness
// against the cost of re-streaming the encrypted dump through a verifier.
const DefaultRestoreVerificationInterval = 168 * time.Hour

// Source is the flattened, validated view of a BackupSource the pipeline can
// act on.
//
// RetentionDays / MinKeep semantics:
//   - -1 means "field absent — fall back to global default at apply time"
//   - 0 means "explicitly disabled by the user; keep forever"
//   - >0 means "delete dumps older than N days, but never below MinKeep"
type Source struct {
	// ObjectName is the BackupSource's metadata.name. It keys the managed
	// CronJob name and the jitter hash, so it must be stable for the life of
	// the source.
	ObjectName string
	Namespace  string
	// CredentialsSecret is the name of the Secret holding the password.
	CredentialsSecret string
	TargetName        string // logical name used in metrics, paths, schedule registration
	DBType            string
	Schedule          string
	AnalyzerEnabled   bool
	// DestinationAllow is the allow-list from spec.destinations.
	// Empty means: fan out to all discovered destinations (default).
	DestinationAllow  []string
	RetentionDays     int
	MinKeep           int
	RowDropThreshold  float64 // -1 = use default
	SizeDropThreshold float64 // -1 = use default
	AnonymizeTables   bool
	// EmptyDumpCheck enables the hard-fail when pre-dump stats show rows but
	// the dump itself contains zero INSERTs. Default true.
	EmptyDumpCheck bool
	// Suspended pauses scheduled runs without deleting the source. The
	// reconciler writes the value into the managed CronJob's Spec.Suspend.
	// Manual triggers ignore Suspend entirely — that's the intended escape
	// hatch for one-off restores.
	Suspended bool
	// RestoreVerificationMode is one of the v1alpha1.Verification* values.
	RestoreVerificationMode string
	// RestoreVerificationInterval is the minimum gap between verifier-runs.
	// 0 means "unset — use DefaultRestoreVerificationInterval at apply time".
	RestoreVerificationInterval time.Duration
	// VerificationImage overrides the verifier-pod image. Empty falls back
	// to a per-DB-type default (see verifier/restore/engine.go).
	VerificationImage string
	// VerificationVolumeSize is the emptyDir sizeLimit for the verifier
	// pod's data volume (e.g. "100Gi"). Empty falls back to a per-mode default.
	VerificationVolumeSize string
	// JitterMinutes controls per-source minute spreading on the materialised
	// CronJob. -1 = unset (jitter only when minute==0). 0 = explicit opt-out.
	// >0 = explicit window. See scheduler.ApplyJitter.
	JitterMinutes int
	// Compression selects the algorithm used before age encryption: "gzip"
	// or "zstd". Stored in meta.json so restore knows the decompressor.
	Compression string
	// TLSCA, when set, names the Secret key holding a CA bundle the worker
	// must materialise to disk before dumping. Only the worker acts on it —
	// see ApplyTLSCAPath.
	TLSCA  *v1alpha1.SecretKeyRef
	Config dumper.Config
}

// AllowsDestination reports whether the given destination name is permitted
// for this source. When the allow-list is empty, every destination is allowed.
func (s *Source) AllowsDestination(name string) bool {
	if len(s.DestinationAllow) == 0 {
		return true
	}
	for _, n := range s.DestinationAllow {
		if n == name {
			return true
		}
	}
	return false
}

// Destination is the flattened view of a BackupDestination plus its
// credentials. The storage factory consumes Data, keyed exactly like the
// legacy destination Secret schema (host, port, bucket, password, ...), so
// no storage backend had to change for the CRD migration.
type Destination struct {
	// ObjectName is the BackupDestination's metadata.name; identical to
	// Name. Kept as a separate field so call sites that mean "the K8s
	// object" read as such.
	ObjectName        string
	Namespace         string
	CredentialsSecret string
	Name              string // logical name used in metrics and allow-lists
	StorageType       string
	Data              map[string][]byte
}

// FilterDestinations returns the subset of destinations the source's
// allow-list permits. An empty allow-list means all destinations pass.
func FilterDestinations(src *Source, all []*Destination) []*Destination {
	if len(src.DestinationAllow) == 0 {
		return all
	}
	out := make([]*Destination, 0, len(all))
	for _, d := range all {
		if src.AllowsDestination(d.Name) {
			out = append(out, d)
		}
	}
	return out
}

func defaultPortFor(dbType string) int {
	switch dbType {
	case v1alpha1.DatabasePostgres:
		return 5432
	case v1alpha1.DatabaseMySQL, v1alpha1.DatabaseMariaDB:
		return 3306
	case v1alpha1.DatabaseMongo:
		return 27017
	case v1alpha1.DatabaseRedis:
		return 6379
	}
	return 0
}
