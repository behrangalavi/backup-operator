package secrets

import (
	"testing"

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
