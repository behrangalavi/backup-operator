package migrate

import (
	"bytes"
	"strings"
	"testing"

	"backup-operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	keyA = "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p"
	keyB = "age1zvkyg2lqzraa2lnjvqej32nkuu0ues2s82hzrye869xeexvn73equnujwj"
)

func legacySource(name string, ann map[string]string, data map[string]string) corev1.Secret {
	s := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "backup",
			Labels:      map[string]string{LabelRole: RoleSource, LabelDBType: "postgres"},
			Annotations: ann,
		},
		Data: map[string][]byte{"host": []byte("db"), "username": []byte("u"), "password": []byte("pw"), "database": []byte("app"), "port": []byte("5433")},
	}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

func legacyDest(name, typ, logical string, data map[string]string) corev1.Secret {
	s := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "backup",
			Labels:      map[string]string{LabelRole: RoleDestination, LabelStorageType: typ},
			Annotations: map[string]string{AnnotationName: logical, AnnotationPathPrefix: "/prod"},
		},
		Data: map[string][]byte{},
	}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

func TestConvertSource_FullAnnotationSet(t *testing.T) {
	s := legacySource("prod-db", map[string]string{
		AnnotationName:                               "prod-users",
		AnnotationSchedule:                           "15 2 * * *",
		AnnotationJitterMinutes:                      "30",
		AnnotationSuspended:                          "true",
		AnnotationDestinations:                       "Hetzner SB, minio",
		AnnotationRetentionDays:                      "0",
		AnnotationMinKeep:                            "5",
		AnnotationCompression:                        "zstd",
		AnnotationAnalyzerEnabled:                    "false",
		AnnotationEmptyDumpCheck:                     "no",
		AnnotationAnonymizeTables:                    "true",
		AnnotationRowDropThreshold:                   "0.3",
		AnnotationSizeDropThreshold:                  "0.75",
		AnnotationRestoreVerificationMode:            "stream-validate",
		AnnotationRestoreVerificationInterval:        "72h",
		AnnotationVerificationImage:                  "postgres:16",
		AnnotationVerificationVolumeSize:             "20Gi",
		AnnotationExtraPrefix + "sslmode":            "verify-full",
		AnnotationExtraPrefix + "sslrootcert":        "/certs/ca.pem",
		AnnotationExtraPrefix + "max-allowed-packet": "1G",
		AnnotationPrefix + "analyzer-enable":         "false", // typo → warning
	}, nil)
	bs, creds, warns, err := ConvertSource(&s, map[string]string{"Hetzner SB": "hetzner-sb"})
	if err != nil {
		t.Fatalf("ConvertSource: %v", err)
	}
	sp := bs.Spec
	if bs.Name != "prod-db" || sp.TargetName != "prod-users" {
		t.Errorf("name/targetName: %q/%q", bs.Name, sp.TargetName)
	}
	if sp.Database.Host != "db" || sp.Database.Name != "app" || sp.Database.Username != "u" || sp.Database.Port == nil || *sp.Database.Port != 5433 {
		t.Errorf("database: %+v", sp.Database)
	}
	if sp.Database.CredentialsSecretRef.Name != "prod-db-credentials" || creds.Name != "prod-db-credentials" || creds.StringData["password"] != "pw" {
		t.Errorf("credentials: ref=%+v secret=%+v", sp.Database.CredentialsSecretRef, creds.StringData)
	}
	if sp.Schedule != "15 2 * * *" || sp.JitterMinutes == nil || *sp.JitterMinutes != 30 || !sp.Suspended {
		t.Errorf("schedule block: %+v", sp)
	}
	if strings.Join(sp.Destinations, ",") != "hetzner-sb,minio" {
		t.Errorf("destinations must be rewritten via renames: %v", sp.Destinations)
	}
	if sp.Retention == nil || sp.Retention.Days == nil || *sp.Retention.Days != 0 || *sp.Retention.MinKeep != 5 {
		t.Errorf("retention: %+v", sp.Retention)
	}
	if sp.Compression != "zstd" {
		t.Errorf("compression: %q", sp.Compression)
	}
	a := sp.Analyzer
	if a == nil || a.Enabled == nil || *a.Enabled || a.EmptyDumpCheck == nil || *a.EmptyDumpCheck || !a.AnonymizeTables ||
		a.RowDropThresholdPercent == nil || *a.RowDropThresholdPercent != 30 || *a.SizeDropThresholdPercent != 75 {
		t.Errorf("analyzer: %+v", a)
	}
	rv := sp.RestoreVerification
	if rv == nil || rv.Mode != "stream-validate" || rv.Interval == nil || rv.Interval.Hours() != 72 || rv.Image != "postgres:16" || rv.VolumeSize == nil || rv.VolumeSize.String() != "20Gi" {
		t.Errorf("verification: %+v", rv)
	}
	if sp.Database.TLS == nil || sp.Database.TLS.Mode != "verify-full" {
		t.Errorf("extra-sslmode must become tls.mode: %+v", sp.Database.TLS)
	}
	if sp.Database.Extra["max-allowed-packet"] != "1G" {
		t.Errorf("other extras must survive: %v", sp.Database.Extra)
	}
	if _, kept := sp.Database.Extra["sslrootcert"]; kept {
		t.Error("file-path TLS options must be dropped with a warning")
	}
	joined := strings.Join(warns, "\n")
	for _, want := range []string{"sslrootcert", "unknown annotation backup.mogenius.io/analyzer-enable"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings must mention %q, got:\n%s", want, joined)
		}
	}
}

func TestConvertSource_MinimalAndErrors(t *testing.T) {
	s := legacySource("tiny", nil, nil)
	bs, _, warns, err := ConvertSource(&s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bs.Spec.TargetName != "" || bs.Spec.Retention != nil || bs.Spec.Analyzer != nil || bs.Spec.RestoreVerification != nil || bs.Spec.Compression != "" {
		t.Errorf("minimal source must not synthesise optional blocks: %+v", bs.Spec)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}

	bad := legacySource("bad", nil, nil)
	bad.Labels[LabelDBType] = "oracle"
	if _, _, _, err := ConvertSource(&bad, nil); err == nil {
		t.Error("unsupported db-type must error")
	}
	nohost := legacySource("nohost", nil, nil)
	delete(nohost.Data, "host")
	if _, _, _, err := ConvertSource(&nohost, nil); err == nil {
		t.Error("missing host must error")
	}
	nopw := legacySource("nopw", nil, nil)
	delete(nopw.Data, "password")
	_, creds, warns, err := ConvertSource(&nopw, nil)
	if err != nil || len(creds.StringData) != 0 || len(warns) == 0 {
		t.Errorf("missing password → empty Secret + warning; got creds=%v warns=%v err=%v", creds.StringData, warns, err)
	}
}

func TestConvertDestination_HetznerAliasAndRename(t *testing.T) {
	s := legacyDest("sb", "hetzner-sftp", "Hetzner SB", map[string]string{
		"host": "u1.your-storagebox.de", "username": "u1", "ssh-private-key": "PEM", "known-hosts": "kh", "insecure-skip-host-verify": "true",
	})
	bd, creds, warns, err := ConvertDestination(&s)
	if err != nil {
		t.Fatalf("ConvertDestination: %v", err)
	}
	if bd.Name != "hetzner-sb" {
		t.Errorf("logical name must be DNS-sanitised: %q", bd.Name)
	}
	if bd.Spec.Type != v1alpha1.StorageSFTP || bd.Spec.SFTP == nil || bd.Spec.SFTP.Port == nil || *bd.Spec.SFTP.Port != 23 {
		t.Errorf("hetzner-sftp must become sftp on port 23: %+v", bd.Spec)
	}
	if !bd.Spec.SFTP.InsecureSkipHostVerify || bd.Spec.SFTP.KnownHosts != "kh" || bd.Spec.PathPrefix != "/prod" {
		t.Errorf("sftp fields: %+v prefix=%q", bd.Spec.SFTP, bd.Spec.PathPrefix)
	}
	if creds.Name != "hetzner-sb-credentials" || creds.StringData["ssh-private-key"] != "PEM" || len(creds.StringData) != 1 {
		t.Errorf("credentials must carry exactly the credential keys: %+v", creds.StringData)
	}
	if !strings.Contains(strings.Join(warns, "\n"), "renamed") {
		t.Errorf("rename must be warned: %v", warns)
	}
}

func TestConvertDestination_S3(t *testing.T) {
	s := legacyDest("minio", "s3", "minio", map[string]string{
		"bucket": "backups", "endpoint": "http://minio:9000", "path-style": "true", "access-key-id": "AK", "secret-access-key": "SK",
	})
	bd, creds, _, err := ConvertDestination(&s)
	if err != nil {
		t.Fatal(err)
	}
	if bd.Spec.S3 == nil || bd.Spec.S3.Bucket != "backups" || !bd.Spec.S3.PathStyle || bd.Spec.S3.Endpoint != "http://minio:9000" {
		t.Errorf("s3 spec: %+v", bd.Spec.S3)
	}
	if creds.StringData["access-key-id"] != "AK" || creds.StringData["secret-access-key"] != "SK" {
		t.Errorf("s3 creds: %v", creds.StringData)
	}
	unsupported := legacyDest("x", "ftp", "x", nil)
	if _, _, _, err := ConvertDestination(&unsupported); err == nil {
		t.Error("unsupported storage-type must error")
	}
}

func TestBuildPlan_EndToEnd(t *testing.T) {
	all := []corev1.Secret{
		legacySource("prod-db", map[string]string{AnnotationDestinations: "My NAS"}, nil),
		legacyDest("nas", "sftp", "My NAS", map[string]string{"host": "nas", "username": "u", "password": "p"}),
		{
			ObjectMeta: metav1.ObjectMeta{Name: "rec-alice", Namespace: "backup", Labels: map[string]string{LabelRole: RoleAgeRecipient}},
			Data:       map[string][]byte{RecipientPublicKeyField: []byte(keyA)},
		},
		{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "backup"}, Data: map[string][]byte{"x": []byte("y")}},
	}
	plan := BuildPlan(all, Options{MergedSecretName: "merged"})
	if len(plan.Sources) != 1 || len(plan.Destinations) != 1 || len(plan.Recipients) != 1 || len(plan.Secrets) != 2 {
		t.Fatalf("plan counts: %d/%d/%d/%d", len(plan.Sources), len(plan.Destinations), len(plan.Recipients), len(plan.Secrets))
	}
	if plan.Destinations[0].Name != "my-nas" || plan.Sources[0].Spec.Destinations[0] != "my-nas" {
		t.Errorf("rename must flow into the source allow-list: dest=%q allow=%v", plan.Destinations[0].Name, plan.Sources[0].Spec.Destinations)
	}
	if plan.Recipients[0].Name != "rec-alice" || plan.Recipients[0].Spec.PublicKey != keyA {
		t.Errorf("recipient: %+v", plan.Recipients[0])
	}
	if strings.Join(plan.Legacy, ",") != "nas,prod-db,rec-alice" {
		t.Errorf("legacy list must name every converted Secret: %v", plan.Legacy)
	}
	var buf bytes.Buffer
	if err := WriteYAML(&buf, plan); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"kind: BackupSource", "kind: BackupDestination", "kind: BackupRecipient", "kind: Secret", "credentialsSecretRef", "password: p"} {
		if !strings.Contains(out, want) {
			t.Errorf("YAML must contain %q", want)
		}
	}
}

func TestBuildPlan_SplitsLegacyMergedSecret(t *testing.T) {
	all := make([]corev1.Secret, 0, 2)
	all = append(all, corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "merged", Namespace: "backup"},
		Data:       map[string][]byte{"AGE_PUBLIC_KEYS": []byte(keyA + "\n# c\n" + keyB + "\nnot-a-key")},
	})
	plan := BuildPlan(all, Options{MergedSecretName: "merged"})
	if len(plan.Recipients) != 2 {
		t.Fatalf("expected 2 recipients from the merged Secret, got %d (%v)", len(plan.Recipients), plan.Warnings)
	}
	if len(plan.Legacy) != 0 {
		t.Error("the merged Secret is operator-managed and must not be listed for deletion")
	}
	if !strings.Contains(strings.Join(plan.Warnings, "\n"), "not an age") {
		t.Errorf("invalid key must be warned: %v", plan.Warnings)
	}
	// With per-recipient Secrets present the merged one is ignored.
	all = append(all, corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rec", Namespace: "backup", Labels: map[string]string{LabelRole: RoleAgeRecipient}},
		Data:       map[string][]byte{RecipientPublicKeyField: []byte(keyA)},
	})
	plan = BuildPlan(all, Options{MergedSecretName: "merged"})
	if len(plan.Recipients) != 1 {
		t.Errorf("per-recipient Secrets take precedence over the merged blob, got %d", len(plan.Recipients))
	}
}

func TestDNSName(t *testing.T) {
	cases := map[string]string{"Hetzner SB": "hetzner-sb", "my_dest": "my-dest", "ok-name": "ok-name", "..": "", "A.B": "a.b"}
	for in, want := range cases {
		if got := dnsName(in); got != want {
			t.Errorf("dnsName(%q) = %q, want %q", in, got, want)
		}
	}
}
