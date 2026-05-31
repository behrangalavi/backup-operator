package factory

import (
	"strings"
	"testing"

	"backup-operator/storage"

	"github.com/go-logr/logr"
)

func TestNewStorage_Unsupported(t *testing.T) {
	_, err := NewStorage("dropbox", "test", storage.SecretData{}, logr.Discard())
	if err == nil {
		t.Fatal("expected error for unsupported storage-type")
	}
	if !strings.Contains(err.Error(), "dropbox") {
		t.Errorf("error should mention the type, got: %v", err)
	}
}

func TestNewStorage_Azure_MissingAccountName(t *testing.T) {
	// Routing check: azure with missing data should reach the azure
	// constructor (missing-field error), not "unsupported type".
	_, err := NewStorage(TypeAzure, "test", storage.SecretData{
		"container": []byte("backups"),
	}, logr.Discard())
	if err == nil {
		t.Fatal("expected error for missing azure account name")
	}
	if strings.Contains(err.Error(), "unsupported") {
		t.Errorf("azure should route to azure constructor, got: %v", err)
	}
}

func TestNewStorage_GCS_MissingBucket(t *testing.T) {
	// Routing check: gcs with missing data should reach the gcs
	// constructor (missing-field error), not "unsupported type".
	_, err := NewStorage(TypeGCS, "test", storage.SecretData{
		"service-account-json": []byte(`{"type":"service_account"}`),
	}, logr.Discard())
	if err == nil {
		t.Fatal("expected error for missing gcs bucket")
	}
	if strings.Contains(err.Error(), "unsupported") {
		t.Errorf("gcs should route to gcs constructor, got: %v", err)
	}
}

func TestNewStorage_S3_MissingBucket(t *testing.T) {
	_, err := NewStorage(TypeS3, "test", storage.SecretData{
		"access-key-id":     []byte("a"),
		"secret-access-key": []byte("s"),
	}, logr.Discard())
	if err == nil {
		t.Fatal("expected error for missing bucket")
	}
}

func TestNewStorage_S3_Valid(t *testing.T) {
	s, err := NewStorage(TypeS3, "prod", storage.SecretData{
		"bucket":            []byte("backups"),
		"access-key-id":     []byte("AKIA"),
		"secret-access-key": []byte("secret"),
	}, logr.Discard())
	if err != nil {
		t.Fatalf("NewStorage(s3) failed: %v", err)
	}
	if got := s.Name(); got != "prod" {
		t.Errorf("Name() = %q, want %q", got, "prod")
	}
}

func TestNewStorage_HetznerSFTPAlias(t *testing.T) {
	// TypeHetznerSFTP is just an alias for TypeSFTP. It should reach the
	// SFTP constructor. We can't provide valid SSH data here so we expect
	// a missing-auth error (which proves routing works).
	_, err := NewStorage(TypeHetznerSFTP, "test", storage.SecretData{
		"host": []byte("sb.hetzner.de"),
		"port": []byte("23"),
	}, logr.Discard())
	if err == nil {
		t.Fatal("expected error for missing SSH auth data")
	}
	// The error should come from the SFTP constructor, not "unsupported type".
	if strings.Contains(err.Error(), "unsupported") {
		t.Errorf("hetzner-sftp should route to sftp constructor, got: %v", err)
	}
}
