package secrets

import (
	"testing"
	"time"

	"backup-operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newSource(mut func(*v1alpha1.BackupSource)) *v1alpha1.BackupSource {
	bs := &v1alpha1.BackupSource{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-db", Namespace: "default"},
		Spec: v1alpha1.BackupSourceSpec{
			Database: v1alpha1.DatabaseSpec{
				Type:                 v1alpha1.DatabasePostgres,
				Host:                 "db.example.com",
				Port:                 ptr32(5432),
				Name:                 "app",
				Username:             "backup",
				CredentialsSecretRef: v1alpha1.CredentialsSecretRef{Name: "prod-db-credentials"},
			},
		},
	}
	if mut != nil {
		mut(bs)
	}
	return bs
}

func creds(data map[string]string) *corev1.Secret {
	s := &corev1.Secret{Data: map[string][]byte{}}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

func ptr32(n int32) *int32 { return &n }
func ptrBool(b bool) *bool { return &b }

func TestFromBackupSource_RejectsTraversalTargetName(t *testing.T) {
	for _, name := range []string{"../other-tenant", "../../etc", "a/b", "foo/../bar", ".hidden", "has space"} {
		bs := newSource(func(b *v1alpha1.BackupSource) { b.Spec.TargetName = name })
		if _, err := FromBackupSource(bs, nil, "0 2 * * *"); err == nil {
			t.Errorf("must reject unsafe target name %q", name)
		}
	}
}

func TestFromBackupSource_AcceptsNormalTargetNames(t *testing.T) {
	for _, name := range []string{"prod-users", "prod_users", "mydb.staging", "db2"} {
		bs := newSource(func(b *v1alpha1.BackupSource) { b.Spec.TargetName = name })
		src, err := FromBackupSource(bs, nil, "0 2 * * *")
		if err != nil {
			t.Errorf("rejected valid target name %q: %v", name, err)
			continue
		}
		if src.TargetName != name || src.Config.Name != name {
			t.Errorf("target name %q not propagated: %+v", name, src)
		}
	}
}

func TestFromBackupSource_TargetDefaultsToObjectName(t *testing.T) {
	src, err := FromBackupSource(newSource(nil), nil, "0 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	if src.TargetName != "prod-db" || src.ObjectName != "prod-db" || src.CredentialsSecret != "prod-db-credentials" {
		t.Errorf("identity fields wrong: %+v", src)
	}
}

func TestFromBackupSource_Defaults(t *testing.T) {
	src, err := FromBackupSource(newSource(nil), nil, "0 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	if !src.AnalyzerEnabled || !src.EmptyDumpCheck || src.Suspended || src.AnonymizeTables {
		t.Errorf("flag defaults wrong: %+v", src)
	}
	if src.RetentionDays != -1 || src.MinKeep != -1 || src.JitterMinutes != -1 {
		t.Errorf("unset ints must be -1 (use operator default): %+v", src)
	}
	if src.RowDropThreshold != -1 || src.SizeDropThreshold != -1 {
		t.Errorf("unset thresholds must be -1: %+v", src)
	}
	if src.RestoreVerificationMode != v1alpha1.VerificationOff || src.RestoreVerificationInterval != 0 {
		t.Errorf("verification defaults wrong: %+v", src)
	}
	if src.Compression != v1alpha1.CompressionGzip {
		t.Errorf("compression default = %q", src.Compression)
	}
	if src.Schedule != "0 2 * * *" {
		t.Errorf("empty spec.schedule must fall back to the operator default, got %q", src.Schedule)
	}
	if src.Config.Password != "" {
		t.Error("no credentials Secret → no password")
	}
}

func TestFromBackupSource_ExplicitValues(t *testing.T) {
	bs := newSource(func(b *v1alpha1.BackupSource) {
		b.Spec.Schedule = "*/15 * * * *"
		b.Spec.JitterMinutes = ptr32(0)
		b.Spec.Suspended = true
		b.Spec.Destinations = []string{"s3-offsite", "sftp-local"}
		b.Spec.Retention = &v1alpha1.RetentionSpec{Days: ptr32(0), MinKeep: ptr32(5)}
		b.Spec.Compression = v1alpha1.CompressionZstd
		b.Spec.Analyzer = &v1alpha1.AnalyzerSpec{
			Enabled:                  ptrBool(false),
			EmptyDumpCheck:           ptrBool(false),
			AnonymizeTables:          true,
			RowDropThresholdPercent:  ptr32(30),
			SizeDropThresholdPercent: ptr32(70),
		}
		b.Spec.RestoreVerification = &v1alpha1.RestoreVerificationSpec{
			Mode:       v1alpha1.VerificationSchemaOnly,
			Interval:   &metav1.Duration{Duration: 30 * time.Minute},
			Image:      "postgres:15.5-alpine",
			VolumeSize: func() *resource.Quantity { q := resource.MustParse("100Gi"); return &q }(),
		}
	})
	src, err := FromBackupSource(bs, nil, "0 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	if src.Schedule != "*/15 * * * *" || src.JitterMinutes != 0 || !src.Suspended {
		t.Errorf("schedule fields: %+v", src)
	}
	if len(src.DestinationAllow) != 2 || !src.AllowsDestination("s3-offsite") || src.AllowsDestination("other") {
		t.Errorf("allow-list: %v", src.DestinationAllow)
	}
	if src.RetentionDays != 0 || src.MinKeep != 5 {
		t.Errorf("retention: days=%d minKeep=%d (0 must mean keep-forever, not unset)", src.RetentionDays, src.MinKeep)
	}
	if src.Compression != v1alpha1.CompressionZstd {
		t.Errorf("compression = %q", src.Compression)
	}
	if src.AnalyzerEnabled || src.EmptyDumpCheck || !src.AnonymizeTables {
		t.Errorf("analyzer flags: %+v", src)
	}
	if src.RowDropThreshold != 0.3 || src.SizeDropThreshold != 0.7 {
		t.Errorf("percent → ratio: row=%v size=%v", src.RowDropThreshold, src.SizeDropThreshold)
	}
	if src.RestoreVerificationMode != v1alpha1.VerificationSchemaOnly || src.RestoreVerificationInterval != 30*time.Minute ||
		src.VerificationImage != "postgres:15.5-alpine" || src.VerificationVolumeSize != "100Gi" {
		t.Errorf("verification: %+v", src)
	}
}

func TestFromBackupSource_CredentialsAndKeyOverrides(t *testing.T) {
	bs := newSource(func(b *v1alpha1.BackupSource) {
		b.Spec.Database.Username = "ignored"
		b.Spec.Database.CredentialsSecretRef = v1alpha1.CredentialsSecretRef{Name: "cnpg-app", PasswordKey: "pw", UsernameKey: "user"}
	})
	src, err := FromBackupSource(bs, creds(map[string]string{"pw": "s3cret", "user": "app-owner"}), "0 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	if src.Config.Password != "s3cret" || src.Config.Username != "app-owner" {
		t.Errorf("key overrides not honoured: user=%q pw=%q", src.Config.Username, src.Config.Password)
	}
	// Default key when PasswordKey is empty.
	src, err = FromBackupSource(newSource(nil), creds(map[string]string{"password": "p"}), "0 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	if src.Config.Password != "p" {
		t.Errorf("default password key not read: %q", src.Config.Password)
	}
}

func TestFromBackupSource_UsernameDeferredWhenInSecret(t *testing.T) {
	// Spec-only flatten with usernameKey set: the username lives in the
	// Secret we weren't given, so the check must not fail the reconciler.
	bs := newSource(func(b *v1alpha1.BackupSource) {
		b.Spec.Database.Username = ""
		b.Spec.Database.CredentialsSecretRef.UsernameKey = "user"
	})
	if _, err := FromBackupSource(bs, nil, "0 2 * * *"); err != nil {
		t.Errorf("username check must be deferred when it lives in the Secret: %v", err)
	}
	// With the Secret present but the key empty, it fails loudly.
	if _, err := FromBackupSource(bs, creds(map[string]string{}), "0 2 * * *"); err == nil {
		t.Error("empty username from Secret must be rejected for postgres")
	}
}

func TestFromBackupSource_RedisAllowsEmptyUsername(t *testing.T) {
	bs := newSource(func(b *v1alpha1.BackupSource) {
		b.Spec.Database.Type = v1alpha1.DatabaseRedis
		b.Spec.Database.Port = nil
		b.Spec.Database.Username = ""
	})
	src, err := FromBackupSource(bs, nil, "0 2 * * *")
	if err != nil {
		t.Fatalf("redis source with empty username should flatten: %v", err)
	}
	if src.Config.Port != 6379 {
		t.Errorf("redis default port = %d", src.Config.Port)
	}
}

func TestFromBackupSource_NonRedisRejectsEmptyUsername(t *testing.T) {
	for _, dbt := range []string{v1alpha1.DatabasePostgres, v1alpha1.DatabaseMySQL, v1alpha1.DatabaseMariaDB, v1alpha1.DatabaseMongo} {
		bs := newSource(func(b *v1alpha1.BackupSource) {
			b.Spec.Database.Type = dbt
			b.Spec.Database.Username = ""
		})
		if _, err := FromBackupSource(bs, nil, "0 2 * * *"); err == nil {
			t.Errorf("%s: expected error for empty username", dbt)
		}
	}
}

func TestFromBackupSource_RejectsStructuralErrors(t *testing.T) {
	cases := map[string]func(*v1alpha1.BackupSource){
		"unsupported type": func(b *v1alpha1.BackupSource) { b.Spec.Database.Type = "oracle" },
		"empty host":       func(b *v1alpha1.BackupSource) { b.Spec.Database.Host = " " },
		"no credentials":   func(b *v1alpha1.BackupSource) { b.Spec.Database.CredentialsSecretRef.Name = "" },
	}
	for name, mut := range cases {
		if _, err := FromBackupSource(newSource(mut), nil, "0 2 * * *"); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestDefaultPortFor(t *testing.T) {
	cases := map[string]int{"postgres": 5432, "mysql": 3306, "mariadb": 3306, "mongo": 27017, "redis": 6379, "unknown": 0}
	for dbt, want := range cases {
		if got := defaultPortFor(dbt); got != want {
			t.Errorf("defaultPortFor(%q) = %d, want %d", dbt, got, want)
		}
	}
}

func TestFromBackupSource_TLSMapping(t *testing.T) {
	pg := newSource(func(b *v1alpha1.BackupSource) {
		b.Spec.Database.TLS = &v1alpha1.DatabaseTLSSpec{Mode: "verify-full", CASecretRef: &v1alpha1.SecretKeyRef{Name: "ca", Key: "ca.crt"}}
		b.Spec.Database.Extra = map[string]string{"foo": "bar"}
	})
	src, err := FromBackupSource(pg, nil, "0 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	if src.Config.Extra["sslmode"] != "verify-full" || src.Config.Extra["foo"] != "bar" {
		t.Errorf("postgres TLS mode must land in Extra[sslmode] alongside user extras: %v", src.Config.Extra)
	}
	if src.TLSCA == nil || src.TLSCA.Name != "ca" {
		t.Errorf("CA ref not carried: %+v", src.TLSCA)
	}
	ApplyTLSCAPath(src, "/tmp/ca.pem")
	if src.Config.Extra["sslrootcert"] != "/tmp/ca.pem" {
		t.Errorf("postgres CA path must land in sslrootcert: %v", src.Config.Extra)
	}

	my := newSource(func(b *v1alpha1.BackupSource) {
		b.Spec.Database.Type = v1alpha1.DatabaseMySQL
		b.Spec.Database.TLS = &v1alpha1.DatabaseTLSSpec{Mode: "true"}
	})
	src, err = FromBackupSource(my, nil, "0 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	if src.Config.Extra["tls"] != "true" {
		t.Errorf("mysql TLS mode must land in Extra[tls]: %v", src.Config.Extra)
	}
	ApplyTLSCAPath(src, "/tmp/ca.pem")
	if src.Config.Extra["ssl-ca"] != "/tmp/ca.pem" {
		t.Errorf("mysql CA path must land in ssl-ca: %v", src.Config.Extra)
	}
}

func TestSource_AllowsDestination(t *testing.T) {
	cases := []struct {
		allow []string
		name  string
		want  bool
	}{
		{nil, "any", true},
		{[]string{}, "any", true},
		{[]string{"a", "b"}, "a", true},
		{[]string{"a", "b"}, "c", false},
	}
	for _, c := range cases {
		s := &Source{DestinationAllow: c.allow}
		if got := s.AllowsDestination(c.name); got != c.want {
			t.Errorf("allow=%v name=%q want=%v got=%v", c.allow, c.name, c.want, got)
		}
	}
}
