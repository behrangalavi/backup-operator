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
)
