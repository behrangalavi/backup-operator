// Package labels holds the storage-layout constants shared by the pipeline,
// the restore CLI, the verifiers and the UI: restore-verification mode
// values, compression algorithms and the encrypted-dump file suffixes.
//
// The backup.mogenius.io/* label and annotation keys that used to live here
// described the legacy Secret-based discovery contract. Discovery now runs
// through the BackupSource / BackupDestination / BackupRecipient CRDs
// (api/v1alpha1); the legacy keys survive only in internal/migrate, which
// converts old Secrets into those objects.
package labels

// MergedRecipientsField is the data key on the operator-managed merged age
// Secret. Newline-separated list of recipients, matching the env-var
// format the worker consumes via `secretKeyRef`.
const MergedRecipientsField = "AGE_PUBLIC_KEYS"

// Restore-verification mode values. RestoreVerificationOff /
// StreamValidate run without extra RBAC. The remaining three (SchemaOnly /
// Sample / Full) are Phase-2 modes that spawn an ephemeral DB pod via
// verifier/ephemeral and require restoreVerification.enableEphemeralPodSpawn=true
// in the chart so the worker SA gets pods: create/get/list/watch/delete in
// its own namespace.
const (
	RestoreVerificationOff            = "off"
	RestoreVerificationStreamValidate = "stream-validate"
	RestoreVerificationSchemaOnly     = "schema-only"
	RestoreVerificationSample         = "sample"
	RestoreVerificationFull           = "full"
)

// Compression algorithm values for the compression annotation.
const (
	CompressionGzip = "gzip"
	CompressionZstd = "zstd"
)

// DumpSuffix returns the file extension for an encrypted dump based on the
// compression algorithm: "sql.zst.age" for zstd, "sql.gz.age" for gzip
// (or empty/unknown, preserving backward compat).
func DumpSuffix(compression string) string {
	if compression == CompressionZstd {
		return "sql.zst.age"
	}
	return "sql.gz.age"
}

// IsDumpSuffix returns true if the path ends with any known encrypted dump
// suffix (.sql.gz.age or .sql.zst.age). Used by retention classification,
// storage scrubber, and destination stats to recognise dump artifacts
// regardless of which compression algorithm was used.
func IsDumpSuffix(path string) bool {
	return hasSuffix(path, ".sql.gz.age") || hasSuffix(path, ".sql.zst.age")
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
