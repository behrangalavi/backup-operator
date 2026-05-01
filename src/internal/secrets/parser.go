package secrets

import (
	"fmt"
	"strconv"
	"strings"

	"backup-operator/dumper"
	"backup-operator/internal/labels"

	corev1 "k8s.io/api/core/v1"
)

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
	if user == "" {
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

	// Surface path-prefix from annotation through into Data so storage impls
	// see one consistent input shape.
	data := make(map[string][]byte, len(s.Data)+1)
	for k, v := range s.Data {
		data[k] = v
	}
	if prefix := s.Annotations[labels.AnnotationPathPrefix]; prefix != "" {
		data["path-prefix"] = []byte(prefix)
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
	return p, nil
}

func defaultPortFor(dbType string) int {
	switch dbType {
	case "postgres":
		return 5432
	case "mysql":
		return 3306
	case "mongo":
		return 27017
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
