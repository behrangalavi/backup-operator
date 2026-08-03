package secrets

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"backup-operator/dumper"
	"backup-operator/internal/labels"

	corev1 "k8s.io/api/core/v1"
)

// validName is the conservative allow-list for logical identifiers (source
// target name, destination name) that flow into object-storage paths, metric
// labels, and CronJob names. It forbids the path separator and any leading
// dot, so a value like "../other-tenant" or "a/b" cannot escape a
// destination's path-prefix once path.Join'd into an object key. Without a
// '/' a literal ".." is inert (path.Clean leaves it in place), so barring the
// separator is sufficient to close the traversal class.
//
// Feature-flag annotations get forgiving fallbacks (see the parseXAnnotation
// helpers), but a structural identifier that decides *where* data is written
// and deleted must fail loudly rather than be silently rewritten — same
// contract as the required host/username/port keys.
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
			return fmt.Errorf("path-prefix %q must not contain a %q segment", v, "..")
		}
	}
	return nil
}

// DefaultRestoreVerificationInterval is used when restore-verification-mode is
// active but no interval annotation is set. Weekly cadence balances signal
// freshness against the cost of re-streaming the encrypted dump through a
// verifier each time.
const DefaultRestoreVerificationInterval = 168 * time.Hour

// Source describes a parsed source Secret that the pipeline can act on.
//
// RetentionDays / MinKeep semantics:
//   - -1 means "annotation absent — fall back to global default at apply time"
//   - 0 means "explicitly disabled by the user; keep forever"
//   - >0 means "delete dumps older than N days, but never below MinKeep"
type Source struct {
	SecretName       string
	Namespace        string
	TargetName       string // logical name used in metrics, paths, schedule registration
	DBType           string
	Schedule         string
	AnalyzerEnabled  bool
	// DestinationAllow is the parsed allow-list from the annotation.
	// Empty means: fan out to all discovered destinations (default).
	DestinationAllow []string
	RetentionDays      int
	MinKeep            int
	RowDropThreshold   float64 // -1 = use default
	SizeDropThreshold  float64 // -1 = use default
	AnonymizeTables    bool
	// EmptyDumpCheck enables the hard-fail when pre-dump stats show rows but
	// the dump itself contains zero INSERTs. Default true. Set the annotation
	// to false on schema-only sources (e.g. an empty template DB).
	EmptyDumpCheck     bool
	// Suspended pauses scheduled runs without deleting the source. The
	// reconciler writes the value into the managed CronJob's Spec.Suspend.
	// Manual triggers (kubectl create job --from=cronjob) ignore Suspend
	// entirely — that's the intended escape hatch for one-off restores.
	Suspended         bool
	// RestoreVerificationMode is one of the labels.RestoreVerification* values.
	// Empty annotation → "off". Unknown values fall back to "off" rather than
	// rejecting the source — a typo on a feature flag must not stop backups.
	RestoreVerificationMode string
	// RestoreVerificationInterval is the minimum gap between verifier-runs.
	// 0 means "annotation absent — use DefaultRestoreVerificationInterval at
	// apply time". Negative durations are coerced to 0 for the same reason.
	RestoreVerificationInterval time.Duration
	// VerificationImage overrides the verifier-pod image. Empty falls
	// back to a per-DB-type default (see verifier/restore/engine.go).
	// Only consulted when RestoreVerificationMode is schema-only / sample / full.
	VerificationImage string
	// VerificationVolumeSize is the emptyDir sizeLimit for the verifier
	// pod's data volume (e.g. "100Gi", "5Gi"). Empty falls back to a
	// per-mode default. Same gating as VerificationImage.
	VerificationVolumeSize string
	// JitterMinutes controls per-source minute spreading on the
	// materialised CronJob. -1 = annotation absent (default behaviour:
	// jitter only when the user wrote minute==0). 0 = explicit opt-out.
	// >0 = explicit window. See scheduler.ApplyJitter for the full
	// decision tree.
	JitterMinutes int
	// Compression selects the algorithm used to compress the dump before
	// age encryption. "gzip" (default) or "zstd". Stored in meta.json so
	// the restore CLI and verifiers know which decompressor to use.
	Compression   string
	Config             dumper.Config
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

// Destination describes a parsed destination Secret. The actual Storage
// implementation is constructed by the storage factory at upload time.
type Destination struct {
	SecretName  string
	Namespace   string
	Name        string // logical name used in metrics
	StorageType string
	Data        map[string][]byte
}

// IsSource returns true for Secrets that should be backed up.
func IsSource(s *corev1.Secret) bool {
	return s.Labels[labels.LabelRole] == labels.RoleSource
}

// IsDestination returns true for Secrets that describe an upload target.
func IsDestination(s *corev1.Secret) bool {
	return s.Labels[labels.LabelRole] == labels.RoleDestination
}

// ParseSource extracts a Source from a Secret. Returns an error with enough
// context that the controller can log it and skip the Secret.
func ParseSource(s *corev1.Secret, defaultSchedule string) (*Source, error) {
	dbType := s.Labels[labels.LabelDBType]
	if dbType == "" {
		return nil, fmt.Errorf("secret %s/%s: missing label %s", s.Namespace, s.Name, labels.LabelDBType)
	}

	host := strings.TrimSpace(string(s.Data["host"]))
	if host == "" {
		return nil, fmt.Errorf("secret %s/%s: missing data key %q", s.Namespace, s.Name, "host")
	}
	user := strings.TrimSpace(string(s.Data["username"]))
	// Redis pre-6 uses password-only AUTH; ACL usernames came in 6.0. Allow
	// empty username for redis sources only — every other DB requires it.
	if user == "" && dbType != "redis" {
		return nil, fmt.Errorf("secret %s/%s: missing data key %q", s.Namespace, s.Name, "username")
	}

	port, err := parsePort(string(s.Data["port"]), defaultPortFor(dbType))
	if err != nil {
		return nil, fmt.Errorf("secret %s/%s: %w", s.Namespace, s.Name, err)
	}

	target := s.Annotations[labels.AnnotationName]
	if target == "" {
		target = s.Name
	}
	if err := validateName("target name", target); err != nil {
		return nil, fmt.Errorf("secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	schedule := s.Annotations[labels.AnnotationSchedule]
	if schedule == "" {
		schedule = defaultSchedule
	}

	return &Source{
		SecretName:       s.Name,
		Namespace:        s.Namespace,
		TargetName:       target,
		DBType:           dbType,
		Schedule:         schedule,
		AnalyzerEnabled:  parseBoolAnnotation(s.Annotations[labels.AnnotationAnalyzerEnabled], true),
		DestinationAllow: parseCSVAnnotation(s.Annotations[labels.AnnotationDestinations]),
		RetentionDays:      parseIntAnnotation(s.Annotations[labels.AnnotationRetentionDays], -1),
		MinKeep:            parseIntAnnotation(s.Annotations[labels.AnnotationMinKeep], -1),
		RowDropThreshold:   parseFloatAnnotation(s.Annotations[labels.AnnotationRowDropThreshold], -1),
		SizeDropThreshold:  parseFloatAnnotation(s.Annotations[labels.AnnotationSizeDropThreshold], -1),
		AnonymizeTables:    parseBoolAnnotation(s.Annotations[labels.AnnotationAnonymizeTables], false),
		EmptyDumpCheck:     parseBoolAnnotation(s.Annotations[labels.AnnotationEmptyDumpCheck], true),
		JitterMinutes:      parseIntAnnotation(s.Annotations[labels.AnnotationJitterMinutes], -1),
		Compression:        parseCompression(s.Annotations[labels.AnnotationCompression]),
		Suspended:          parseBoolAnnotation(s.Annotations[labels.AnnotationSuspended], false),
		RestoreVerificationMode:     parseRestoreVerificationMode(s.Annotations[labels.AnnotationRestoreVerificationMode]),
		RestoreVerificationInterval: parseDurationAnnotation(s.Annotations[labels.AnnotationRestoreVerificationInterval]),
		VerificationImage:           strings.TrimSpace(s.Annotations[labels.AnnotationVerificationImage]),
		VerificationVolumeSize:      strings.TrimSpace(s.Annotations[labels.AnnotationVerificationVolumeSize]),
		Config: dumper.Config{
			Name:     target,
			Host:     host,
			Port:     port,
			Database: strings.TrimSpace(string(s.Data["database"])),
			Username: user,
			Password: string(s.Data["password"]),
			Extra:    extraFromAnnotations(s.Annotations),
		},
	}, nil
}

// ParseDestination extracts a Destination from a Secret.
func ParseDestination(s *corev1.Secret) (*Destination, error) {
	storageType := s.Labels[labels.LabelStorageType]
	if storageType == "" {
		return nil, fmt.Errorf("secret %s/%s: missing label %s", s.Namespace, s.Name, labels.LabelStorageType)
	}

	name := s.Annotations[labels.AnnotationName]
	if name == "" {
		name = s.Name
	}
	if err := validateName("destination name", name); err != nil {
		return nil, fmt.Errorf("secret %s/%s: %w", s.Namespace, s.Name, err)
	}

	// Surface path-prefix from annotation through into Data so storage impls
	// see one consistent input shape. The annotation wins over a data key.
	data := make(map[string][]byte, len(s.Data)+1)
	for k, v := range s.Data {
		data[k] = v
	}
	if prefix := s.Annotations[labels.AnnotationPathPrefix]; prefix != "" {
		data["path-prefix"] = []byte(prefix)
	}
	// Validate the EFFECTIVE prefix regardless of whether it came from the
	// annotation or directly from Secret data — every backend reads
	// data["path-prefix"] and joins it into object paths, so a ".." there
	// climbs out of the destination's isolation root just as an annotation
	// ".." would. Validating only the annotation left the data-key path open.
	if prefix := string(data["path-prefix"]); prefix != "" {
		if err := validatePathPrefix(prefix); err != nil {
			return nil, fmt.Errorf("secret %s/%s: %w", s.Namespace, s.Name, err)
		}
	}

	return &Destination{
		SecretName:  s.Name,
		Namespace:   s.Namespace,
		Name:        name,
		StorageType: storageType,
		Data:        data,
	}, nil
}

func parsePort(s string, def int) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	p, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid port %q: %w", s, err)
	}
	if p < 1 || p > 65535 {
		return 0, fmt.Errorf("port %d out of range (1-65535)", p)
	}
	return p, nil
}

func defaultPortFor(dbType string) int {
	switch dbType {
	case "postgres":
		return 5432
	case "mysql", "mariadb":
		return 3306
	case "mongo":
		return 27017
	case "redis":
		return 6379
	}
	return 0
}

// parseBoolAnnotation accepts standard truthy/falsy strings; anything
// unrecognised falls back to the supplied default rather than rejecting the
// whole Secret — a typo on a feature flag should not stop backups running.
func parseBoolAnnotation(v string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	}
	return def
}

// parseIntAnnotation parses a decimal integer; an empty value or a malformed
// value falls back to def — same forgiveness rule as parseBoolAnnotation:
// a typo on a flag must not stop backups running.
func parseIntAnnotation(v string, def int) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// parseFloatAnnotation parses a decimal float; empty or malformed values
// fall back to def — same forgiveness rule as the other parsers.
func parseFloatAnnotation(v string, def float64) float64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// parseCSVAnnotation splits a comma-separated annotation, trimming spaces
// and dropping empties. Returns nil for empty input so callers can use
// len() == 0 as the "no constraint" signal.
func parseCSVAnnotation(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseRestoreVerificationMode normalises the annotation value. Unknown
// values fall back to "off" — typos on the flag must not silently enable a
// mode the user did not pick.
func parseRestoreVerificationMode(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "", labels.RestoreVerificationOff:
		return labels.RestoreVerificationOff
	case labels.RestoreVerificationStreamValidate,
		labels.RestoreVerificationSchemaOnly,
		labels.RestoreVerificationSample,
		labels.RestoreVerificationFull:
		return v
	}
	return labels.RestoreVerificationOff
}

// parseDurationAnnotation accepts any Go-style duration string (e.g. "168h",
// "30m", "1h30m"). Empty / malformed → 0, signalling "use default at apply
// time". Forgiveness rule: a typo here must not reject the Secret.
func parseDurationAnnotation(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// extraFromAnnotations exposes any backup.mogenius.io/extra-* annotations
// to the Dumper.Config.Extra map without coupling parser to specific keys.
func extraFromAnnotations(ann map[string]string) map[string]string {
	const prefix = "backup.mogenius.io/extra-"
	if len(ann) == 0 {
		return nil
	}
	out := make(map[string]string)
	for k, v := range ann {
		if strings.HasPrefix(k, prefix) {
			out[strings.TrimPrefix(k, prefix)] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// WarnUnknownAnnotations returns the list of annotations with the project
// prefix that are not recognised. Callers should log these as warnings so
// typos like "analyzer-enable" (instead of "analyzer-enabled") are visible.
func WarnUnknownAnnotations(annotations map[string]string) []string {
	var unknown []string
	for k := range annotations {
		if !strings.HasPrefix(k, labels.AnnotationPrefix) {
			continue
		}
		// extra-* annotations are intentionally open-ended.
		if strings.HasPrefix(k, labels.AnnotationPrefix+"extra-") {
			continue
		}
		if !labels.KnownAnnotations[k] {
			unknown = append(unknown, k)
		}
	}
	return unknown
}

func parseCompression(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case labels.CompressionZstd:
		return labels.CompressionZstd
	default:
		return labels.CompressionGzip
	}
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
