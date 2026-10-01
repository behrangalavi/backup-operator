// Package v1alpha1 contains the backup.mogenius.io/v1alpha1 API types.
//
// Discovery used to be driven by labelled Secrets. Each kind here replaces
// one of those roles: BackupSource (role=source), BackupDestination
// (role=destination) and BackupRecipient (role=age-recipient). Only the
// credentials remain in Secrets, referenced by name from the CR spec.
//
// +kubebuilder:object:generate=true
// +groupName=backup.mogenius.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group/version the CRDs in this package belong to.
	GroupVersion = schema.GroupVersion{Group: "backup.mogenius.io", Version: "v1alpha1"}

	// SchemeBuilder registers the types below with a runtime.Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds every type in this package to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// Kind names, kept as constants so OwnerReferences and Event refs built by
// hand can't drift from the registered types.
const (
	KindBackupSource      = "BackupSource"
	KindBackupDestination = "BackupDestination"
	KindBackupRecipient   = "BackupRecipient"
)
