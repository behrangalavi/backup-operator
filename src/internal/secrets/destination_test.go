package secrets

import (
	"testing"

	"backup-operator/api/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newDest(typ string, mut func(*v1alpha1.BackupDestination)) *v1alpha1.BackupDestination {
	bd := &v1alpha1.BackupDestination{
		ObjectMeta: metav1.ObjectMeta{Name: "dest", Namespace: "default"},
		Spec: v1alpha1.BackupDestinationSpec{
			Type:                 typ,
			CredentialsSecretRef: v1alpha1.LocalSecretRef{Name: "dest-credentials"},
		},
	}
	switch typ {
	case v1alpha1.StorageS3:
		bd.Spec.S3 = &v1alpha1.S3Spec{Bucket: "backups", Endpoint: "http://minio:9000", Region: "eu", PathStyle: true}
	case v1alpha1.StorageSFTP:
		bd.Spec.SFTP = &v1alpha1.SFTPSpec{Host: "sb.example", Port: ptr32(23), Username: "u1", KnownHosts: "kh", InsecureSkipHostVerify: false}
	case v1alpha1.StorageFTPS:
		bd.Spec.FTPS = &v1alpha1.FTPSSpec{Host: "nas", Username: "u", TLSMode: "implicit", InsecureSkipCertVerify: true}
	case v1alpha1.StorageAzure:
		bd.Spec.Azure = &v1alpha1.AzureSpec{AccountName: "acct", Container: "c"}
	case v1alpha1.StorageGCS:
		bd.Spec.GCS = &v1alpha1.GCSSpec{Bucket: "g"}
	}
	if mut != nil {
		mut(bd)
	}
	return bd
}

func TestFromBackupDestination_RejectsTraversalPathPrefix(t *testing.T) {
	bd := newDest(v1alpha1.StorageS3, func(b *v1alpha1.BackupDestination) { b.Spec.PathPrefix = "/cluster-prod/../other" })
	if _, err := FromBackupDestination(bd, nil); err == nil {
		t.Error("must reject a pathPrefix with a '..' segment")
	}
}

func TestFromBackupDestination_AllowsSlashedPathPrefix(t *testing.T) {
	bd := newDest(v1alpha1.StorageS3, func(b *v1alpha1.BackupDestination) { b.Spec.PathPrefix = "/cluster-prod/eu" })
	d, err := FromBackupDestination(bd, nil)
	if err != nil {
		t.Fatalf("rejected a valid slashed pathPrefix: %v", err)
	}
	if string(d.Data["path-prefix"]) != "/cluster-prod/eu" {
		t.Errorf("path-prefix not surfaced into Data: %v", d.Data)
	}
}

func TestFromBackupDestination_S3DataShape(t *testing.T) {
	d, err := FromBackupDestination(newDest(v1alpha1.StorageS3, nil), creds(map[string]string{
		"access-key-id": "AK", "secret-access-key": "SK", "host": "must-not-leak",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"bucket": "backups", "endpoint": "http://minio:9000", "region": "eu", "path-style": "true", "access-key-id": "AK", "secret-access-key": "SK"}
	for k, v := range want {
		if string(d.Data[k]) != v {
			t.Errorf("Data[%s] = %q, want %q", k, d.Data[k], v)
		}
	}
	if _, leaked := d.Data["host"]; leaked {
		t.Error("non-credential keys in the Secret must not reach Data — the spec owns connection parameters")
	}
	if d.Name != "dest" || d.ObjectName != "dest" || d.StorageType != "s3" || d.CredentialsSecret != "dest-credentials" {
		t.Errorf("identity fields: %+v", d)
	}
}

func TestFromBackupDestination_SFTPDataShape(t *testing.T) {
	d, err := FromBackupDestination(newDest(v1alpha1.StorageSFTP, nil), creds(map[string]string{"ssh-private-key": "PEM"}))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"host": "sb.example", "port": "23", "username": "u1", "known-hosts": "kh", "ssh-private-key": "PEM"} {
		if string(d.Data[k]) != v {
			t.Errorf("Data[%s] = %q, want %q", k, d.Data[k], v)
		}
	}
	if _, ok := d.Data["insecure-skip-host-verify"]; ok {
		t.Error("false booleans must be absent, not \"false\"")
	}
}

func TestFromBackupDestination_FTPSAzureGCS(t *testing.T) {
	d, err := FromBackupDestination(newDest(v1alpha1.StorageFTPS, nil), creds(map[string]string{"password": "p"}))
	if err != nil {
		t.Fatal(err)
	}
	if string(d.Data["tls-mode"]) != "implicit" || string(d.Data["insecure-skip-cert-verify"]) != "true" || string(d.Data["password"]) != "p" {
		t.Errorf("ftps Data: %v", d.Data)
	}
	d, err = FromBackupDestination(newDest(v1alpha1.StorageAzure, nil), creds(map[string]string{"account-key": "k"}))
	if err != nil {
		t.Fatal(err)
	}
	if string(d.Data["account-name"]) != "acct" || string(d.Data["container"]) != "c" || string(d.Data["account-key"]) != "k" {
		t.Errorf("azure Data: %v", d.Data)
	}
	d, err = FromBackupDestination(newDest(v1alpha1.StorageGCS, nil), creds(map[string]string{"service-account-json": "{}"}))
	if err != nil {
		t.Fatal(err)
	}
	if string(d.Data["bucket"]) != "g" || string(d.Data["service-account-json"]) != "{}" {
		t.Errorf("gcs Data: %v", d.Data)
	}
}

func TestFromBackupDestination_MissingBlockOrType(t *testing.T) {
	bd := newDest(v1alpha1.StorageS3, func(b *v1alpha1.BackupDestination) { b.Spec.S3 = nil })
	if _, err := FromBackupDestination(bd, nil); err == nil {
		t.Error("type s3 without spec.s3 must be rejected")
	}
	bd = newDest("ftp", nil)
	if _, err := FromBackupDestination(bd, nil); err == nil {
		t.Error("unsupported type must be rejected")
	}
	bd = newDest(v1alpha1.StorageS3, func(b *v1alpha1.BackupDestination) { b.Spec.CredentialsSecretRef.Name = "" })
	if _, err := FromBackupDestination(bd, nil); err == nil {
		t.Error("empty credentialsSecretRef must be rejected")
	}
}

func TestCredentialKeys_CoverEveryType(t *testing.T) {
	for _, typ := range []string{v1alpha1.StorageSFTP, v1alpha1.StorageFTPS, v1alpha1.StorageS3, v1alpha1.StorageAzure, v1alpha1.StorageGCS} {
		if len(CredentialKeys(typ)) == 0 {
			t.Errorf("%s has no credential keys — the UI mask and the migrator would miss its secrets", typ)
		}
	}
	if CredentialKeys("nope") != nil {
		t.Error("unknown type must have no credential keys")
	}
}

func TestFilterDestinations(t *testing.T) {
	all := []*Destination{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	got := FilterDestinations(&Source{DestinationAllow: []string{"c", "a"}}, all)
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Errorf("FilterDestinations = %v", got)
	}
	if len(FilterDestinations(&Source{}, all)) != 3 {
		t.Error("empty allow-list must pass everything")
	}
}
