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
)

// Restore-verification mode values.
const (
	RestoreVerificationOff            = "off"
	RestoreVerificationStreamValidate = "stream-validate"
	// Phase 2 — reserved, not yet implemented:
	RestoreVerificationSchemaOnly = "schema-only"
	RestoreVerificationSample     = "sample"
	RestoreVerificationFull       = "full"
)
