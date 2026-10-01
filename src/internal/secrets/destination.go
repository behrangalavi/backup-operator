package secrets

import (
	"fmt"
	"strconv"
	"strings"

	"backup-operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
)

// CredentialKeys lists the Secret data keys a storage type reads as
// credentials. The UI masks exactly these on GET and the migration tool
// moves exactly these out of a legacy destination Secret, so adding a
// backend means extending this one table.
func CredentialKeys(storageType string) []string {
	switch storageType {
	case v1alpha1.StorageSFTP:
		return []string{"password", "ssh-private-key"}
	case v1alpha1.StorageFTPS:
		return []string{"password"}
	case v1alpha1.StorageS3:
		return []string{"access-key-id", "secret-access-key"}
	case v1alpha1.StorageAzure:
		return []string{"account-key"}
	case v1alpha1.StorageGCS:
		return []string{"service-account-json"}
	}
	return nil
}

// FromBackupDestination flattens a BackupDestination plus its credentials
// Secret into the Data map the storage factory consumes. creds may be nil
// for display-only callers; the storage factory will then reject the
// Destination at construction time, which is the right failure for a
// caller that tried to connect without credentials.
func FromBackupDestination(bd *v1alpha1.BackupDestination, creds *corev1.Secret) (*Destination, error) {
	spec := bd.Spec
	where := fmt.Sprintf("backupdestination %s/%s", bd.Namespace, bd.Name)

	if err := validateName("destination name", bd.Name); err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	if strings.TrimSpace(spec.CredentialsSecretRef.Name) == "" {
		return nil, fmt.Errorf("%s: spec.credentialsSecretRef.name is required", where)
	}

	data := make(map[string][]byte, 12)
	// Credentials first, spec second: a credentials Secret can never override
	// a connection parameter the CR declares.
	if creds != nil {
		for _, k := range CredentialKeys(spec.Type) {
			if v, ok := creds.Data[k]; ok && len(v) > 0 {
				data[k] = v
			}
		}
	}

	set := func(k, v string) {
		if v = strings.TrimSpace(v); v != "" {
			data[k] = []byte(v)
		}
	}
	setPort := func(p *int32) {
		if p != nil {
			data["port"] = []byte(strconv.Itoa(int(*p)))
		}
	}
	setBool := func(k string, b bool) {
		if b {
			data[k] = []byte("true")
		}
	}

	switch spec.Type {
	case v1alpha1.StorageSFTP:
		if spec.SFTP == nil {
			return nil, fmt.Errorf("%s: spec.sftp is required for type sftp", where)
		}
		set("host", spec.SFTP.Host)
		setPort(spec.SFTP.Port)
		set("username", spec.SFTP.Username)
		set("known-hosts", spec.SFTP.KnownHosts)
		setBool("insecure-skip-host-verify", spec.SFTP.InsecureSkipHostVerify)
	case v1alpha1.StorageFTPS:
		if spec.FTPS == nil {
			return nil, fmt.Errorf("%s: spec.ftps is required for type ftps", where)
		}
		set("host", spec.FTPS.Host)
		setPort(spec.FTPS.Port)
		set("username", spec.FTPS.Username)
		set("tls-mode", spec.FTPS.TLSMode)
		setBool("insecure-skip-cert-verify", spec.FTPS.InsecureSkipCertVerify)
	case v1alpha1.StorageS3:
		if spec.S3 == nil {
			return nil, fmt.Errorf("%s: spec.s3 is required for type s3", where)
		}
		set("bucket", spec.S3.Bucket)
		set("endpoint", spec.S3.Endpoint)
		set("region", spec.S3.Region)
		setBool("path-style", spec.S3.PathStyle)
	case v1alpha1.StorageAzure:
		if spec.Azure == nil {
			return nil, fmt.Errorf("%s: spec.azure is required for type azure", where)
		}
		set("account-name", spec.Azure.AccountName)
		set("container", spec.Azure.Container)
	case v1alpha1.StorageGCS:
		if spec.GCS == nil {
			return nil, fmt.Errorf("%s: spec.gcs is required for type gcs", where)
		}
		set("bucket", spec.GCS.Bucket)
	default:
		return nil, fmt.Errorf("%s: unsupported storage type %q", where, spec.Type)
	}

	if prefix := strings.TrimSpace(spec.PathPrefix); prefix != "" {
		if err := validatePathPrefix(prefix); err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		data["path-prefix"] = []byte(prefix)
	}

	return &Destination{
		ObjectName:        bd.Name,
		Namespace:         bd.Namespace,
		CredentialsSecret: spec.CredentialsSecretRef.Name,
		Name:              bd.Name,
		StorageType:       spec.Type,
		Data:              data,
	}, nil
}
