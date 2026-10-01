package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BackupRecipientSpec holds one age public key. The operator merges every
// recipient in the namespace into the Secret worker pods mount as
// AGE_PUBLIC_KEYS; the private half never enters the cluster.
type BackupRecipientSpec struct {
	// PublicKey is a single age X25519 recipient (age1...).
	// +kubebuilder:validation:Pattern=`^age1[a-z0-9]{58}$`
	PublicKey string `json:"publicKey"`
}

// BackupRecipient is one party that must be able to decrypt backups.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=brcp,categories=backup
// +kubebuilder:printcolumn:name="Public Key",type=string,JSONPath=`.spec.publicKey`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type BackupRecipient struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec BackupRecipientSpec `json:"spec"`
}

// BackupRecipientList is a list of BackupRecipient.
//
// +kubebuilder:object:root=true
type BackupRecipientList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BackupRecipient `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupRecipient{}, &BackupRecipientList{})
}
