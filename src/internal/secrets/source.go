package secrets

import (
	"fmt"
	"strings"

	"backup-operator/api/v1alpha1"
	"backup-operator/dumper"

	corev1 "k8s.io/api/core/v1"
)

// FromBackupSource flattens a BackupSource into a Source. creds is the
// Secret named by spec.database.credentialsSecretRef; it may be nil for
// callers that never connect to the database (the CronJob reconciler, the
// metrics refresher, the UI list views) — the result then carries an empty
// password and, when usernameKey is set, an empty username.
//
// Structural problems (unsupported type, missing host, unsafe target name)
// return an error so the caller skips the object loudly; the CRD schema
// already rejects most of them at admission, this is the second line.
func FromBackupSource(bs *v1alpha1.BackupSource, creds *corev1.Secret, defaultSchedule string) (*Source, error) {
	spec := bs.Spec
	db := spec.Database
	where := fmt.Sprintf("backupsource %s/%s", bs.Namespace, bs.Name)

	if defaultPortFor(db.Type) == 0 {
		return nil, fmt.Errorf("%s: unsupported database type %q", where, db.Type)
	}
	host := strings.TrimSpace(db.Host)
	if host == "" {
		return nil, fmt.Errorf("%s: spec.database.host is required", where)
	}

	target := strings.TrimSpace(spec.TargetName)
	if target == "" {
		target = bs.Name
	}
	if err := validateName("target name", target); err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}

	ref := db.CredentialsSecretRef
	if strings.TrimSpace(ref.Name) == "" {
		return nil, fmt.Errorf("%s: spec.database.credentialsSecretRef.name is required", where)
	}
	passwordKey := ref.PasswordKey
	if passwordKey == "" {
		passwordKey = "password"
	}

	user := strings.TrimSpace(db.Username)
	var password string
	if creds != nil {
		if ref.UsernameKey != "" {
			user = strings.TrimSpace(string(creds.Data[ref.UsernameKey]))
		}
		password = string(creds.Data[passwordKey])
	}
	// Redis pre-6 uses password-only AUTH; ACL usernames came in 6.0. Allow
	// an empty username for redis sources only — every other DB requires it.
	// When the username lives in the Secret and we weren't given the Secret,
	// the check has to wait for the worker.
	usernameDeferred := creds == nil && ref.UsernameKey != ""
	if user == "" && db.Type != v1alpha1.DatabaseRedis && !usernameDeferred {
		return nil, fmt.Errorf("%s: username is required for %s (set spec.database.username or credentialsSecretRef.usernameKey)", where, db.Type)
	}

	port := defaultPortFor(db.Type)
	if db.Port != nil {
		port = int(*db.Port)
	}

	schedule := strings.TrimSpace(spec.Schedule)
	if schedule == "" {
		schedule = defaultSchedule
	}

	src := &Source{
		ObjectName:              bs.Name,
		Namespace:               bs.Namespace,
		CredentialsSecret:       ref.Name,
		TargetName:              target,
		DBType:                  db.Type,
		Schedule:                schedule,
		AnalyzerEnabled:         true,
		DestinationAllow:        append([]string(nil), spec.Destinations...),
		RetentionDays:           -1,
		MinKeep:                 -1,
		RowDropThreshold:        -1,
		SizeDropThreshold:       -1,
		EmptyDumpCheck:          true,
		Suspended:               spec.Suspended,
		RestoreVerificationMode: v1alpha1.VerificationOff,
		JitterMinutes:           -1,
		Compression:             v1alpha1.CompressionGzip,
		Config: dumper.Config{
			Name:     target,
			Host:     host,
			Port:     port,
			Database: strings.TrimSpace(db.Name),
			Username: user,
			Password: password,
			Extra:    extraFor(db),
		},
	}
	applyOptionalSpec(src, spec)
	return src, nil
}

// applyOptionalSpec copies the optional spec blocks (jitter, retention,
// compression, analyzer, restore verification, TLS CA) onto a Source whose
// required fields are already set. Unset pointers keep the "-1 / default"
// sentinels the pipeline resolves at apply time.
func applyOptionalSpec(src *Source, spec v1alpha1.BackupSourceSpec) {
	db := spec.Database
	if len(src.DestinationAllow) == 0 {
		src.DestinationAllow = nil
	}
	if spec.JitterMinutes != nil {
		src.JitterMinutes = int(*spec.JitterMinutes)
	}
	if r := spec.Retention; r != nil {
		if r.Days != nil {
			src.RetentionDays = int(*r.Days)
		}
		if r.MinKeep != nil {
			src.MinKeep = int(*r.MinKeep)
		}
	}
	if spec.Compression == v1alpha1.CompressionZstd {
		src.Compression = v1alpha1.CompressionZstd
	}
	if a := spec.Analyzer; a != nil {
		if a.Enabled != nil {
			src.AnalyzerEnabled = *a.Enabled
		}
		if a.EmptyDumpCheck != nil {
			src.EmptyDumpCheck = *a.EmptyDumpCheck
		}
		src.AnonymizeTables = a.AnonymizeTables
		if a.RowDropThresholdPercent != nil {
			src.RowDropThreshold = float64(*a.RowDropThresholdPercent) / 100
		}
		if a.SizeDropThresholdPercent != nil {
			src.SizeDropThreshold = float64(*a.SizeDropThresholdPercent) / 100
		}
	}
	if rv := spec.RestoreVerification; rv != nil {
		if rv.Mode != "" {
			src.RestoreVerificationMode = rv.Mode
		}
		if rv.Interval != nil && rv.Interval.Duration > 0 {
			src.RestoreVerificationInterval = rv.Interval.Duration
		}
		src.VerificationImage = strings.TrimSpace(rv.Image)
		if rv.VolumeSize != nil && !rv.VolumeSize.IsZero() {
			src.VerificationVolumeSize = rv.VolumeSize.String()
		}
	}
	if db.TLS != nil && db.TLS.CASecretRef != nil {
		ca := *db.TLS.CASecretRef
		src.TLSCA = &ca
	}
}

// extraFor builds the dumper's Extra map from spec.database.extra plus the
// engine-specific TLS mode key, so dumpers keep reading the same keys the
// legacy extra-* annotations populated.
func extraFor(db v1alpha1.DatabaseSpec) map[string]string {
	out := make(map[string]string, len(db.Extra)+1)
	for k, v := range db.Extra {
		out[k] = v
	}
	if db.TLS != nil {
		if mode := strings.TrimSpace(db.TLS.Mode); mode != "" {
			out[tlsModeKey(db.Type)] = mode
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// tlsModeKey is the Extra key each dumper reads its TLS mode from.
func tlsModeKey(dbType string) string {
	if dbType == v1alpha1.DatabasePostgres {
		return "sslmode"
	}
	return "tls"
}

// ApplyTLSCAPath points the dumper at a CA bundle the caller has written to
// disk. The worker calls this after materialising Source.TLSCA; it's a
// no-op for engines whose dumper has no CA knob (mongo, redis).
func ApplyTLSCAPath(src *Source, path string) {
	if src.Config.Extra == nil {
		src.Config.Extra = map[string]string{}
	}
	switch src.DBType {
	case v1alpha1.DatabasePostgres:
		src.Config.Extra["sslrootcert"] = path
	case v1alpha1.DatabaseMySQL, v1alpha1.DatabaseMariaDB:
		src.Config.Extra["ssl-ca"] = path
	}
}
