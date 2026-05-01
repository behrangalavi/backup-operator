package factory

import (
	"fmt"

	"backup-operator/storage"
	"backup-operator/storage/s3"
	"backup-operator/storage/sftp"

	"github.com/go-logr/logr"
)

const (
	TypeSFTP        = "sftp"
	TypeHetznerSFTP = "hetzner-sftp" // alias
	TypeS3          = "s3"
)

// NewStorage creates the right Storage for the given storage-type label.
// Add new backends (azure, gcs, ...) by implementing storage.Storage and
// registering them here. Never branch on storage type outside this factory.
func NewStorage(storageType, name string, data storage.SecretData, logger logr.Logger) (storage.Storage, error) {
	switch storageType {
	case TypeSFTP, TypeHetznerSFTP:
		return sftp.New(name, data, logger.WithName("sftp"))
	case TypeS3:
		return s3.New(name, data, logger.WithName("s3"))
	default:
		return nil, fmt.Errorf("unsupported storage-type %q", storageType)
	}
}
