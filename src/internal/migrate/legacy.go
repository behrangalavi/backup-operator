// Package migrate converts the pre-CRD discovery model — Secrets labelled
// backup.mogenius.io/role with configuration in annotations — into
// BackupSource / BackupDestination / BackupRecipient objects plus
// credentials-only Secrets. It is the one place the legacy label and
// annotation keys still exist; nothing in the operator reads them any more.
package migrate

// Legacy label keys.
const (
	LabelRole        = "backup.mogenius.io/role"
	LabelDBType      = "backup.mogenius.io/db-type"
	LabelStorageType = "backup.mogenius.io/storage-type"

	RoleSource       = "source"
	RoleDestination  = "destination"
	RoleAgeRecipient = "age-recipient"
)

// Legacy annotation keys.
const (
	AnnotationPrefix = "backup.mogenius.io/"

	AnnotationName                        = "backup.mogenius.io/name"
	AnnotationSchedule                    = "backup.mogenius.io/schedule"
	AnnotationPathPrefix                  = "backup.mogenius.io/path-prefix"
	AnnotationAnalyzerEnabled             = "backup.mogenius.io/analyzer-enabled"
	AnnotationDestinations                = "backup.mogenius.io/destinations"
	AnnotationRetentionDays               = "backup.mogenius.io/retention-days"
	AnnotationMinKeep                     = "backup.mogenius.io/min-keep"
	AnnotationSuspended                   = "backup.mogenius.io/suspended"
	AnnotationRowDropThreshold            = "backup.mogenius.io/row-drop-threshold"
	AnnotationSizeDropThreshold           = "backup.mogenius.io/size-drop-threshold"
	AnnotationAnonymizeTables             = "backup.mogenius.io/anonymize-tables"
	AnnotationEmptyDumpCheck              = "backup.mogenius.io/empty-dump-check"
	AnnotationJitterMinutes               = "backup.mogenius.io/jitter-minutes"
	AnnotationCompression                 = "backup.mogenius.io/compression"
	AnnotationRestoreVerificationMode     = "backup.mogenius.io/restore-verification-mode"
	AnnotationRestoreVerificationInterval = "backup.mogenius.io/restore-verification-interval"
	AnnotationVerificationImage           = "backup.mogenius.io/verification-image"
	AnnotationVerificationVolumeSize      = "backup.mogenius.io/verification-volume-size"
	AnnotationExtraPrefix                 = "backup.mogenius.io/extra-"

	// RecipientPublicKeyField is the data key a legacy per-recipient Secret
	// stored its age public key under.
	RecipientPublicKeyField = "public-key"
)

var knownAnnotations = map[string]bool{
	AnnotationName:                        true,
	AnnotationSchedule:                    true,
	AnnotationPathPrefix:                  true,
	AnnotationAnalyzerEnabled:             true,
	AnnotationDestinations:                true,
	AnnotationRetentionDays:               true,
	AnnotationMinKeep:                     true,
	AnnotationSuspended:                   true,
	AnnotationRowDropThreshold:            true,
	AnnotationSizeDropThreshold:           true,
	AnnotationAnonymizeTables:             true,
	AnnotationEmptyDumpCheck:              true,
	AnnotationJitterMinutes:               true,
	AnnotationCompression:                 true,
	AnnotationRestoreVerificationMode:     true,
	AnnotationRestoreVerificationInterval: true,
	AnnotationVerificationImage:           true,
	AnnotationVerificationVolumeSize:      true,
}
