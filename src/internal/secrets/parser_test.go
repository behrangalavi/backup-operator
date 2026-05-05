package secrets

import (
	"testing"
	"time"

	"backup-operator/internal/labels"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newSourceSecret(annotations map[string]string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prod-db",
			Namespace: "default",
			Labels: map[string]string{
				labels.LabelRole:   labels.RoleSource,
				labels.LabelDBType: "postgres",
			},
			Annotations: annotations,
		},
		Data: map[string][]byte{
			"host":     []byte("db.example.com"),
			"port":     []byte("5432"),
			"username": []byte("backup"),
			"password": []byte("s3cret"),
			"database": []byte("app"),
		},
	}
}

func TestParseSource_AnalyzerEnabled_DefaultTrue(t *testing.T) {
	src, err := ParseSource(newSourceSecret(nil), "0 2 * * *")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !src.AnalyzerEnabled {
		t.Error("default should be true")
	}
}

func TestParseSource_AnalyzerEnabled_ExplicitFalse(t *testing.T) {
	for _, v := range []string{"false", "0", "no", "OFF"} {
		src, err := ParseSource(newSourceSecret(map[string]string{
			labels.AnnotationAnalyzerEnabled: v,
		}), "0 2 * * *")
		if err != nil {
			t.Fatalf("value %q: %v", v, err)
		}
		if src.AnalyzerEnabled {
			t.Errorf("value %q: expected false", v)
		}
	}
}

func TestParseSource_AnalyzerEnabled_TypoFallsBackToDefault(t *testing.T) {
	src, err := ParseSource(newSourceSecret(map[string]string{
		labels.AnnotationAnalyzerEnabled: "trueeee",
	}), "0 2 * * *")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !src.AnalyzerEnabled {
		t.Error("typo should fall back to default true, not silently disable")
	}
}

func TestParseSource_EmptyDumpCheck_DefaultTrue(t *testing.T) {
	src, err := ParseSource(newSourceSecret(nil), "0 2 * * *")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !src.EmptyDumpCheck {
		t.Error("EmptyDumpCheck default should be true")
	}
}

func TestParseSource_EmptyDumpCheck_ExplicitFalse(t *testing.T) {
	src, err := ParseSource(newSourceSecret(map[string]string{
		labels.AnnotationEmptyDumpCheck: "false",
	}), "0 2 * * *")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if src.EmptyDumpCheck {
		t.Error("EmptyDumpCheck explicit false must be honored")
	}
}

func TestParseSource_DestinationAllow(t *testing.T) {
	src, err := ParseSource(newSourceSecret(map[string]string{
		labels.AnnotationDestinations: "s3-offsite, sftp-local ,, ",
	}), "0 2 * * *")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []string{"s3-offsite", "sftp-local"}
	if len(src.DestinationAllow) != len(want) {
		t.Fatalf("want %v got %v", want, src.DestinationAllow)
	}
	for i, w := range want {
		if src.DestinationAllow[i] != w {
			t.Errorf("[%d] want %q got %q", i, w, src.DestinationAllow[i])
		}
	}
}

func TestParseSource_ThresholdDefaults(t *testing.T) {
	src, err := ParseSource(newSourceSecret(nil), "0 2 * * *")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if src.RowDropThreshold != -1 {
		t.Errorf("RowDropThreshold default should be -1, got %v", src.RowDropThreshold)
	}
	if src.SizeDropThreshold != -1 {
		t.Errorf("SizeDropThreshold default should be -1, got %v", src.SizeDropThreshold)
	}
}

func TestParseSource_ThresholdExplicit(t *testing.T) {
	src, err := ParseSource(newSourceSecret(map[string]string{
		labels.AnnotationRowDropThreshold:  "0.3",
		labels.AnnotationSizeDropThreshold: "0.7",
	}), "0 2 * * *")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if src.RowDropThreshold != 0.3 {
		t.Errorf("RowDropThreshold = %v, want 0.3", src.RowDropThreshold)
	}
	if src.SizeDropThreshold != 0.7 {
		t.Errorf("SizeDropThreshold = %v, want 0.7", src.SizeDropThreshold)
	}
}

func TestParseSource_ThresholdTypoFallsBack(t *testing.T) {
	src, err := ParseSource(newSourceSecret(map[string]string{
		labels.AnnotationRowDropThreshold: "not-a-number",
	}), "0 2 * * *")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if src.RowDropThreshold != -1 {
		t.Errorf("RowDropThreshold should fall back to -1 on typo, got %v", src.RowDropThreshold)
	}
}

func TestParsePort_Range(t *testing.T) {
	cases := []struct {
		input string
		def   int
		want  int
		err   bool
	}{
		{"5432", 0, 5432, false},
		{"", 5432, 5432, false},
		{"0", 0, 0, true},
		{"-1", 0, 0, true},
		{"65536", 0, 0, true},
		{"99999", 0, 0, true},
		{"1", 0, 1, false},
		{"65535", 0, 65535, false},
		{"abc", 0, 0, true},
	}
	for _, c := range cases {
		got, err := parsePort(c.input, c.def)
		if c.err && err == nil {
			t.Errorf("parsePort(%q, %d) expected error", c.input, c.def)
		}
		if !c.err && err != nil {
			t.Errorf("parsePort(%q, %d) unexpected error: %v", c.input, c.def, err)
		}
		if !c.err && got != c.want {
			t.Errorf("parsePort(%q, %d) = %d, want %d", c.input, c.def, got, c.want)
		}
	}
}

func TestParseSource_InvalidPort(t *testing.T) {
	sec := newSourceSecret(nil)
	sec.Data["port"] = []byte("99999")
	_, err := ParseSource(sec, "0 2 * * *")
	if err == nil {
		t.Error("expected error for out-of-range port")
	}
}

func TestParseSource_RedisAllowsEmptyUsername(t *testing.T) {
	sec := newSourceSecret(nil)
	sec.Labels[labels.LabelDBType] = "redis"
	sec.Data["username"] = []byte("")
	src, err := ParseSource(sec, "0 2 * * *")
	if err != nil {
		t.Fatalf("redis source with empty username should parse: %v", err)
	}
	if src.DBType != "redis" {
		t.Errorf("DBType = %q, want redis", src.DBType)
	}
}

func TestParseSource_NonRedisRejectsEmptyUsername(t *testing.T) {
	for _, dbt := range []string{"postgres", "mysql", "mariadb", "mongo"} {
		sec := newSourceSecret(nil)
		sec.Labels[labels.LabelDBType] = dbt
		sec.Data["username"] = []byte("")
		if _, err := ParseSource(sec, "0 2 * * *"); err == nil {
			t.Errorf("%s: expected error for empty username", dbt)
		}
	}
}

func TestDefaultPortFor(t *testing.T) {
	cases := map[string]int{
		"postgres": 5432,
		"mysql":    3306,
		"mariadb":  3306,
		"mongo":    27017,
		"redis":    6379,
		"unknown":  0,
	}
	for dbt, want := range cases {
		if got := defaultPortFor(dbt); got != want {
			t.Errorf("defaultPortFor(%q) = %d, want %d", dbt, got, want)
		}
	}
}

func TestParseSource_RestoreVerificationMode_DefaultOff(t *testing.T) {
	src, err := ParseSource(newSourceSecret(nil), "0 2 * * *")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if src.RestoreVerificationMode != labels.RestoreVerificationOff {
		t.Errorf("default mode = %q, want %q", src.RestoreVerificationMode, labels.RestoreVerificationOff)
	}
}

func TestParseSource_RestoreVerificationMode_ExplicitValid(t *testing.T) {
	for _, mode := range []string{
		labels.RestoreVerificationOff,
		labels.RestoreVerificationStreamValidate,
		labels.RestoreVerificationSchemaOnly,
		labels.RestoreVerificationSample,
		labels.RestoreVerificationFull,
	} {
		src, err := ParseSource(newSourceSecret(map[string]string{
			labels.AnnotationRestoreVerificationMode: mode,
		}), "0 2 * * *")
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if src.RestoreVerificationMode != mode {
			t.Errorf("mode %q: got %q", mode, src.RestoreVerificationMode)
		}
	}
}

func TestParseSource_RestoreVerificationMode_TypoFallsBackOff(t *testing.T) {
	src, err := ParseSource(newSourceSecret(map[string]string{
		labels.AnnotationRestoreVerificationMode: "stream",
	}), "0 2 * * *")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if src.RestoreVerificationMode != labels.RestoreVerificationOff {
		t.Errorf("typo should fall back to off, got %q", src.RestoreVerificationMode)
	}
}

func TestParseSource_RestoreVerificationInterval_Defaults(t *testing.T) {
	cases := []struct {
		name string
		ann  map[string]string
		want time.Duration
	}{
		{"absent", nil, 0},
		{"empty", map[string]string{labels.AnnotationRestoreVerificationInterval: ""}, 0},
		{"168h", map[string]string{labels.AnnotationRestoreVerificationInterval: "168h"}, 168 * time.Hour},
		{"30m", map[string]string{labels.AnnotationRestoreVerificationInterval: "30m"}, 30 * time.Minute},
		{"malformed-falls-back", map[string]string{labels.AnnotationRestoreVerificationInterval: "weekly"}, 0},
		{"negative-clamped", map[string]string{labels.AnnotationRestoreVerificationInterval: "-1h"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src, err := ParseSource(newSourceSecret(c.ann), "0 2 * * *")
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if src.RestoreVerificationInterval != c.want {
				t.Errorf("got %v, want %v", src.RestoreVerificationInterval, c.want)
			}
		})
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
