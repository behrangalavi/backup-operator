package labels

// Discovery is via Kubernetes labels and annotations on Secrets.
// Labels select Secrets; annotations carry per-target configuration.
const (
	LabelRole        = "backup.mogenius.io/role"
	LabelDBType      = "backup.mogenius.io/db-type"
	LabelStorageType = "backup.mogenius.io/storage-type"

	RoleSource      = "source"
	RoleDestination = "destination"

	AnnotationName             = "backup.mogenius.io/name"
	AnnotationSchedule         = "backup.mogenius.io/schedule"
	AnnotationPathPrefix       = "backup.mogenius.io/path-prefix"
	AnnotationAnalyzerEnabled  = "backup.mogenius.io/analyzer-enabled"
	AnnotationDestinations     = "backup.mogenius.io/destinations"
	AnnotationRetentionDays    = "backup.mogenius.io/retention-days"
	AnnotationMinKeep          = "backup.mogenius.io/min-keep"
	AnnotationRowDropThreshold  = "backup.mogenius.io/row-drop-threshold"
	AnnotationSizeDropThreshold = "backup.mogenius.io/size-drop-threshold"
	AnnotationAnonymizeTables   = "backup.mogenius.io/anonymize-tables"
	AnnotationEmptyDumpCheck    = "backup.mogenius.io/empty-dump-check"

	// Restore-Verification annotations control the post-upload check that
	// proves the encrypted dump is still decryptable and parseable. The
	// worker generates an ephemeral age keypair per verifier-run, adds its
	// public half as a second recipient, runs the verifier in-process, then
	// the pod terminates and the private key is gone. The DR recipient is
	// untouched and can still decrypt forever — see §18 ADR.
	AnnotationRestoreVerificationMode     = "backup.mogenius.io/restore-verification-mode"
	AnnotationRestoreVerificationInterval = "backup.mogenius.io/restore-verification-interval"

	// Phase-2-only: only consulted when restore-verification-mode is
	// schema-only / sample / full (i.e. modes that spawn an ephemeral
	// DB pod). stream-validate ignores both.
	AnnotationVerificationImage      = "backup.mogenius.io/verification-image"
	AnnotationVerificationVolumeSize = "backup.mogenius.io/verification-volume-size"
)

// AnnotationPrefix is the common prefix for all project annotations.
const AnnotationPrefix = "backup.mogenius.io/"

// KnownAnnotations is the set of annotations the operator understands.
// Used by the parser to warn about likely typos.
var KnownAnnotations = map[string]bool{
	AnnotationName:                        true,
	AnnotationSchedule:                    true,
	AnnotationPathPrefix:                  true,
	AnnotationAnalyzerEnabled:             true,
	AnnotationDestinations:                true,
	AnnotationRetentionDays:               true,
	AnnotationMinKeep:                     true,
	AnnotationRowDropThreshold:            true,
	AnnotationSizeDropThreshold:           true,
	AnnotationAnonymizeTables:             true,
	AnnotationEmptyDumpCheck:              true,
	AnnotationRestoreVerificationMode:     true,
	AnnotationRestoreVerificationInterval: true,
	AnnotationVerificationImage:           true,
	AnnotationVerificationVolumeSize:      true,
}

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
