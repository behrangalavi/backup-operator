package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Storage backend types accepted by BackupDestinationSpec.Type. The legacy
// "hetzner-sftp" alias is gone — use sftp with port 23.
const (
	StorageSFTP  = "sftp"
	StorageFTPS  = "ftps"
	StorageS3    = "s3"
	StorageAzure = "azure"
	StorageGCS   = "gcs"
)

// ConditionReachable is True while the operator can list the destination.
// It flips to False on the first refresh tick that fails to read it.
const ConditionReachable = "Reachable"

// BackupDestinationSpec declares one upload target. Exactly one backend
// block must be set and must match Type; the credentials Secret carries
// the keys that backend needs (see docs/crds.md §6.5).
//
// +kubebuilder:validation:XValidation:rule="self.type != 'sftp' || has(self.sftp)",message="spec.sftp is required when type is sftp"
// +kubebuilder:validation:XValidation:rule="self.type != 'ftps' || has(self.ftps)",message="spec.ftps is required when type is ftps"
// +kubebuilder:validation:XValidation:rule="self.type != 's3' || has(self.s3)",message="spec.s3 is required when type is s3"
// +kubebuilder:validation:XValidation:rule="self.type != 'azure' || has(self.azure)",message="spec.azure is required when type is azure"
// +kubebuilder:validation:XValidation:rule="self.type != 'gcs' || has(self.gcs)",message="spec.gcs is required when type is gcs"
// +kubebuilder:validation:XValidation:rule="!has(self.pathPrefix) || !(self.pathPrefix == '..' || self.pathPrefix.startsWith('../') || self.pathPrefix.endsWith('/..') || self.pathPrefix.contains('/../'))",message="spec.pathPrefix must not contain a '..' segment"
type BackupDestinationSpec struct {
	// Type selects the storage backend.
	// +kubebuilder:validation:Enum=sftp;ftps;s3;azure;gcs
	Type string `json:"type"`

	// PathPrefix is prepended to every object path. May contain '/' as the
	// tenant/cluster isolation root; '..' segments are rejected.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	PathPrefix string `json:"pathPrefix,omitempty"`

	// CredentialsSecretRef names the Secret holding the backend's
	// credentials: password or ssh-private-key (sftp), password (ftps),
	// access-key-id + secret-access-key (s3), account-key (azure),
	// service-account-json (gcs).
	CredentialsSecretRef LocalSecretRef `json:"credentialsSecretRef"`

	// +optional
	SFTP *SFTPSpec `json:"sftp,omitempty"`
	// +optional
	FTPS *FTPSSpec `json:"ftps,omitempty"`
	// +optional
	S3 *S3Spec `json:"s3,omitempty"`
	// +optional
	Azure *AzureSpec `json:"azure,omitempty"`
	// +optional
	GCS *GCSSpec `json:"gcs,omitempty"`
}

// LocalSecretRef names a Secret in the same namespace.
type LocalSecretRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// SFTPSpec covers generic SFTP and Hetzner Storage Boxes (port 23).
type SFTPSpec struct {
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port *int32 `json:"port,omitempty"`
	// +kubebuilder:validation:MinLength=1
	Username string `json:"username"`
	// KnownHosts is ssh-keyscan output. Without it the connection is
	// refused unless insecureSkipHostVerify is set.
	// +optional
	KnownHosts string `json:"knownHosts,omitempty"`
	// +optional
	InsecureSkipHostVerify bool `json:"insecureSkipHostVerify,omitempty"`
}

// FTPSSpec covers FTP over TLS for NAS firmware without SFTP.
type FTPSSpec struct {
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port *int32 `json:"port,omitempty"`
	// +kubebuilder:validation:MinLength=1
	Username string `json:"username"`
	// TLSMode is explicit (port 21 + AUTH TLS) or implicit (port 990).
	// +optional
	// +kubebuilder:validation:Enum=explicit;implicit
	// +kubebuilder:default=explicit
	TLSMode string `json:"tlsMode,omitempty"`
	// +optional
	InsecureSkipCertVerify bool `json:"insecureSkipCertVerify,omitempty"`
}

// S3Spec covers AWS S3 and every S3-compatible store.
type S3Spec struct {
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`
	// Endpoint is required for non-AWS providers (MinIO, Hetzner Object
	// Storage, R2, B2, Wasabi). Leave empty for AWS.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// +optional
	Region string `json:"region,omitempty"`
	// PathStyle forces path-style addressing (MinIO and friends).
	// +optional
	PathStyle bool `json:"pathStyle,omitempty"`
}

// AzureSpec covers Azure Blob Storage with shared-key auth.
type AzureSpec struct {
	// +kubebuilder:validation:MinLength=1
	AccountName string `json:"accountName"`
	// +kubebuilder:validation:MinLength=1
	Container string `json:"container"`
}

// GCSSpec covers Google Cloud Storage with a service-account key.
type GCSSpec struct {
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`
}

// BackupDestinationStatus is written by the operator's metrics refresher.
type BackupDestinationStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions: Reachable.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// LastError is the sanitized message of the most recent failed probe.
	// +optional
	LastError string `json:"lastError,omitempty"`
}

// BackupDestination is one storage backend that every (allowed) source
// fans out to.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=bdst,categories=backup
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Prefix",type=string,JSONPath=`.spec.pathPrefix`
// +kubebuilder:printcolumn:name="Reachable",type=string,JSONPath=`.status.conditions[?(@.type=="Reachable")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type BackupDestination struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupDestinationSpec   `json:"spec"`
	Status BackupDestinationStatus `json:"status,omitempty"`
}

// BackupDestinationList is a list of BackupDestination.
//
// +kubebuilder:object:root=true
type BackupDestinationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BackupDestination `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupDestination{}, &BackupDestinationList{})
}
