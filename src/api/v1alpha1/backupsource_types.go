package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Database engine types accepted by DatabaseSpec.Type. Mirror the factory
// constants in dumper/factory; the enum on the field is what the API server
// enforces, these are for Go callers.
const (
	DatabasePostgres = "postgres"
	DatabaseMySQL    = "mysql"
	DatabaseMariaDB  = "mariadb"
	DatabaseMongo    = "mongo"
	DatabaseRedis    = "redis"
)

// Restore-verification modes. See CLAUDE.md §6.2.
const (
	VerificationOff            = "off"
	VerificationStreamValidate = "stream-validate"
	VerificationSchemaOnly     = "schema-only"
	VerificationSample         = "sample"
	VerificationFull           = "full"
)

// Compression algorithms applied before age encryption.
const (
	CompressionGzip = "gzip"
	CompressionZstd = "zstd"
)

// Condition types written to BackupSource.Status.Conditions.
const (
	// ConditionReady is True once the managed CronJob exists and matches
	// the spec. False with reason InvalidSpec when the spec cannot be
	// turned into a runnable source (bad name, missing host, ...).
	ConditionReady = "Ready"
	// ConditionLastRunSucceeded reflects the most recent run found in
	// storage. Unknown until the first meta.json lands.
	ConditionLastRunSucceeded = "LastRunSucceeded"
	// ConditionVerified reflects the most recent restore-verification
	// verdict. Absent while verification is off.
	ConditionVerified = "Verified"
)

// Run status values for BackupSourceStatus.LastRunStatus.
const (
	RunStatusSuccess = "success"
	RunStatusFailed  = "failed"
)

// BackupSourceSpec declares one database to back up: where it is, how to
// authenticate, when to run, and which safety checks to apply.
type BackupSourceSpec struct {
	// TargetName is the logical name used in storage paths, metric labels
	// and CronJob names. Defaults to metadata.name. Only set this when a
	// source was migrated from the legacy Secret model and must keep its
	// existing storage prefix — renaming it orphans the run history.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]*$`
	TargetName string `json:"targetName,omitempty"`

	// Database describes the connection and the credentials Secret.
	Database DatabaseSpec `json:"database"`

	// Schedule is a standard 5-field cron expression. Empty uses the
	// operator's DEFAULT_SCHEDULE.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// JitterMinutes spreads the cron minute across an N-minute window via a
	// deterministic per-source hash. Unset: jitter only when the schedule's
	// minute field is literally "0". 0 pins the schedule exactly. Multi-fire
	// schedules (*/15, 0,30, ranges) are never jittered.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=60
	JitterMinutes *int32 `json:"jitterMinutes,omitempty"`

	// Suspended pauses scheduled runs (Spec.Suspend on the CronJob) without
	// deleting anything. Manual triggers still run.
	// +optional
	Suspended bool `json:"suspended,omitempty"`

	// Destinations is an allow-list of BackupDestination names. Empty fans
	// out to every destination in the namespace.
	// +optional
	// +listType=set
	Destinations []string `json:"destinations,omitempty"`

	// Retention controls pruning of old artifacts.
	// +optional
	Retention *RetentionSpec `json:"retention,omitempty"`

	// Compression algorithm applied to the dump before encryption.
	// +optional
	// +kubebuilder:validation:Enum=gzip;zstd
	// +kubebuilder:default=gzip
	Compression string `json:"compression,omitempty"`

	// Analyzer tunes the semantic dump analysis.
	// +optional
	Analyzer *AnalyzerSpec `json:"analyzer,omitempty"`

	// RestoreVerification configures the periodic prove-it-restores check.
	// +optional
	RestoreVerification *RestoreVerificationSpec `json:"restoreVerification,omitempty"`
}

// DatabaseSpec is the connection half of a source. Everything here is
// non-secret; the password lives in the referenced Secret.
type DatabaseSpec struct {
	// Type selects the dumper.
	// +kubebuilder:validation:Enum=postgres;mysql;mariadb;mongo;redis
	Type string `json:"type"`

	// Host is the database hostname or IP.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// Port defaults per engine (5432, 3306, 27017, 6379).
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port *int32 `json:"port,omitempty"`

	// Name is the database to dump. Required for postgres/mysql/mariadb.
	// Optional for mongo (all non-system DBs) and redis (DB index or
	// comma-separated list; narrows stats only).
	// +optional
	Name string `json:"name,omitempty"`

	// Username for the connection. Required for every engine except redis.
	// Ignored when credentialsSecretRef.usernameKey is set.
	// +optional
	Username string `json:"username,omitempty"`

	// CredentialsSecretRef points at the Secret holding the password.
	CredentialsSecretRef CredentialsSecretRef `json:"credentialsSecretRef"`

	// TLS configures transport encryption to the database.
	// +optional
	TLS *DatabaseTLSSpec `json:"tls,omitempty"`

	// Extra passes engine-specific options to the dumper (e.g.
	// max-allowed-packet for MySQL, authSource/replicaSet for Mongo).
	// Replaces the legacy backup.mogenius.io/extra-<key> annotations.
	// +optional
	Extra map[string]string `json:"extra,omitempty"`
}

// CredentialsSecretRef references a Secret in the same namespace. The key
// overrides let a source point at a Secret the database itself produced
// (CloudNativePG, Bitnami charts) instead of copying the password.
type CredentialsSecretRef struct {
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// PasswordKey is the data key holding the password.
	// +optional
	// +kubebuilder:default=password
	PasswordKey string `json:"passwordKey,omitempty"`

	// UsernameKey, when set, reads the username from the Secret instead of
	// spec.database.username.
	// +optional
	UsernameKey string `json:"usernameKey,omitempty"`
}

// SecretKeyRef addresses a single key in a Secret of the same namespace.
type SecretKeyRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// DatabaseTLSSpec is passed through to the engine's native TLS knob:
// postgres sslmode, mysql/mariadb --ssl-mode / --ssl, redis --tls.
type DatabaseTLSSpec struct {
	// Mode is engine-specific: postgres accepts libpq sslmode values
	// (disable, prefer, require, verify-ca, verify-full); mysql/mariadb and
	// redis accept true/false plus verify-ca/verify-identity for mysql.
	// +optional
	Mode string `json:"mode,omitempty"`

	// CASecretRef supplies a CA bundle. The worker materialises it to a file
	// inside the pod and hands the path to the dump tool and the stats
	// connection, which is what makes verify-ca / verify-full possible.
	// +optional
	CASecretRef *SecretKeyRef `json:"caSecretRef,omitempty"`
}

// RetentionSpec bounds how long artifacts are kept.
type RetentionSpec struct {
	// Days deletes dumps older than N days. 0 keeps forever. Unset uses the
	// operator default.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Days *int32 `json:"days,omitempty"`

	// MinKeep is the safety floor: never delete below this many newest
	// dumps regardless of age. Unset uses the operator default.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MinKeep *int32 `json:"minKeep,omitempty"`
}

// AnalyzerSpec tunes the content-level checks.
type AnalyzerSpec struct {
	// Enabled toggles pre-dump stats collection and the run-to-run
	// comparison. Default true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// RowDropThresholdPercent flags a table whose row count fell below this
	// percentage of the previous run. Default 50.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	RowDropThresholdPercent *int32 `json:"rowDropThresholdPercent,omitempty"`

	// SizeDropThresholdPercent flags a dump whose size fell below this
	// percentage of the previous run. Default 50.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	SizeDropThresholdPercent *int32 `json:"sizeDropThresholdPercent,omitempty"`

	// AnonymizeTables hashes table names in meta.json.
	// +optional
	AnonymizeTables bool `json:"anonymizeTables,omitempty"`

	// EmptyDumpCheck hard-fails a run whose dump has no data although the
	// source has rows. Default true; disable for schema-only sources.
	// +optional
	EmptyDumpCheck *bool `json:"emptyDumpCheck,omitempty"`
}

// RestoreVerificationSpec configures the periodic decrypt-and-restore proof.
type RestoreVerificationSpec struct {
	// Mode selects how deep the verification goes. Phase-2 modes
	// (schema-only, sample, full) spawn an ephemeral DB pod and need the
	// worker RBAC from restoreVerification.enableEphemeralPodSpawn.
	// +optional
	// +kubebuilder:validation:Enum=off;stream-validate;schema-only;sample;full
	// +kubebuilder:default=off
	Mode string `json:"mode,omitempty"`

	// Interval is the minimum gap between verifier runs. Default 168h.
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// Image overrides the verifier DB pod image (Phase 2 only).
	// +optional
	Image string `json:"image,omitempty"`

	// VolumeSize is the emptyDir sizeLimit of the verifier pod (Phase 2
	// only). Defaults per mode: 1Gi / 5Gi / 50Gi.
	// +optional
	VolumeSize *resource.Quantity `json:"volumeSize,omitempty"`
}

// BackupSourceStatus is written by the operator only. The worker never
// touches it — everything here is reconstructed from the CronJob and from
// the meta.json sidecars in storage, which keeps worker RBAC minimal.
type BackupSourceStatus struct {
	// ObservedGeneration is the spec generation the status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions: Ready, LastRunSucceeded, Verified.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// CronJobName is the managed batch/v1.CronJob.
	// +optional
	CronJobName string `json:"cronJobName,omitempty"`

	// MaterializedSchedule is the post-jitter cron expression the CronJob
	// actually fires on.
	// +optional
	MaterializedSchedule string `json:"materializedSchedule,omitempty"`

	// NextScheduleTime is the next fire time computed from the materialized
	// schedule. Absent while suspended.
	// +optional
	NextScheduleTime *metav1.Time `json:"nextScheduleTime,omitempty"`

	// LastRunTime is the start time of the most recent run (any outcome).
	// +optional
	LastRunTime *metav1.Time `json:"lastRunTime,omitempty"`

	// LastSuccessTime is the start time of the most recent successful run.
	// +optional
	LastSuccessTime *metav1.Time `json:"lastSuccessTime,omitempty"`

	// LastRunStatus is "success" or "failed" for the most recent run.
	// +optional
	LastRunStatus string `json:"lastRunStatus,omitempty"`

	// LastRunError carries the sanitized failure message of the most recent
	// run when it failed.
	// +optional
	LastRunError string `json:"lastRunError,omitempty"`

	// LastDumpSizeBytes is the encrypted size of the most recent successful
	// dump.
	// +optional
	LastDumpSizeBytes int64 `json:"lastDumpSizeBytes,omitempty"`

	// LastRunAnomalies is the analyzer anomaly count of the most recent run.
	// +optional
	LastRunAnomalies int32 `json:"lastRunAnomalies,omitempty"`

	// SchemaChangedAt is when the schema fingerprint last changed.
	// +optional
	SchemaChangedAt *metav1.Time `json:"schemaChangedAt,omitempty"`

	// Destinations lists the per-destination outcome of the most recent run.
	// +optional
	// +listType=map
	// +listMapKey=name
	Destinations []DestinationRunStatus `json:"destinations,omitempty"`

	// RestoreVerification is the most recent verifier verdict.
	// +optional
	RestoreVerification *RestoreVerificationStatus `json:"restoreVerification,omitempty"`
}

// DestinationRunStatus is one destination's result in the most recent run.
type DestinationRunStatus struct {
	Name string `json:"name"`
	// Status is "success" or "failed".
	// +optional
	Status string `json:"status,omitempty"`
	// +optional
	LastSuccessTime *metav1.Time `json:"lastSuccessTime,omitempty"`
	// +optional
	Error string `json:"error,omitempty"`
}

// RestoreVerificationStatus mirrors meta.json's restoreVerification block.
type RestoreVerificationStatus struct {
	// +optional
	Mode string `json:"mode,omitempty"`
	// Verdict is match, mismatch or skipped.
	// +optional
	Verdict string `json:"verdict,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// +optional
	Error string `json:"error,omitempty"`
}

// BackupSource is one database the operator backs up on a schedule.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=bsrc,categories=backup
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.database.type`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.status.materializedSchedule`
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspended`
// +kubebuilder:printcolumn:name="Last Run",type=string,JSONPath=`.status.lastRunStatus`
// +kubebuilder:printcolumn:name="Last Success",type=date,JSONPath=`.status.lastSuccessTime`
// +kubebuilder:printcolumn:name="Verified",type=string,JSONPath=`.status.restoreVerification.verdict`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type BackupSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupSourceSpec   `json:"spec"`
	Status BackupSourceStatus `json:"status,omitempty"`
}

// BackupSourceList is a list of BackupSource.
//
// +kubebuilder:object:root=true
type BackupSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BackupSource `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupSource{}, &BackupSourceList{})
}
