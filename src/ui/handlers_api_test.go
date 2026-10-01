package ui

import (
	"testing"
	"time"

	"backup-operator/api/v1alpha1"
)

func TestValidateK8sName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"prod-db", true},
		{"my.backup.source", true},
		{"a", true},
		{"abc123", true},
		{"", false},       // empty not accepted by regex
		{"-start", false}, // can't start with dash
		{"end-", false},   // can't end with dash
		{"UPPER", false},  // must be lowercase
		{"has space", false},
		{"has_underscore", false},
		{string(make([]byte, 254)), false}, // too long
	}
	for _, c := range cases {
		msg := validateK8sName(c.name)
		if c.ok && msg != "" {
			t.Errorf("validateK8sName(%q) unexpected error: %s", c.name, msg)
		}
		if !c.ok && msg == "" {
			t.Errorf("validateK8sName(%q) expected error", c.name)
		}
	}
}

func TestValidatePort(t *testing.T) {
	cases := []struct {
		port string
		ok   bool
	}{
		{"5432", true},
		{"1", true},
		{"65535", true},
		{"0", false},
		{"-1", false},
		{"65536", false},
		{"abc", false},
		{"", false},
	}
	for _, c := range cases {
		msg := validatePort(c.port)
		if c.ok && msg != "" {
			t.Errorf("validatePort(%q) unexpected error: %s", c.port, msg)
		}
		if !c.ok && msg == "" {
			t.Errorf("validatePort(%q) expected error", c.port)
		}
	}
}

func TestValidateCronSchedule(t *testing.T) {
	cases := []struct {
		schedule string
		ok       bool
	}{
		{"0 2 * * *", true},
		{"*/5 * * * *", true},
		{"0 0 1 1 0", true},
		{"0 2 * *", false},         // only 4 fields
		{"0 2 * * * *", false},     // 6 fields
		{"", false},                // empty
		{"every 5 minutes", false}, // not cron
	}
	for _, c := range cases {
		msg := validateCronSchedule(c.schedule)
		if c.ok && msg != "" {
			t.Errorf("validateCronSchedule(%q) unexpected error: %s", c.schedule, msg)
		}
		if !c.ok && msg == "" {
			t.Errorf("validateCronSchedule(%q) expected error", c.schedule)
		}
	}
}

func TestApplySourceRequest_VerificationFields(t *testing.T) {
	var spec v1alpha1.BackupSourceSpec
	msg := applySourceRequest(&spec, sourceRequest{
		Name:                        "prod-db",
		RestoreVerificationMode:     "stream-validate",
		RestoreVerificationInterval: "168h",
		VerificationImage:           "postgres:15.5-alpine",
		VerificationVolumeSize:      "100Gi",
	}, false)
	if msg != "" {
		t.Fatalf("unexpected validation message: %s", msg)
	}
	rv := spec.RestoreVerification
	if rv == nil || rv.Mode != "stream-validate" || rv.Interval == nil || rv.Interval.Duration != 168*time.Hour ||
		rv.Image != "postgres:15.5-alpine" || rv.VolumeSize == nil || rv.VolumeSize.String() != "100Gi" {
		t.Errorf("restoreVerification = %+v", rv)
	}
}

func TestApplySourceRequest_VerificationOmittedWhenEmpty(t *testing.T) {
	var spec v1alpha1.BackupSourceSpec
	if msg := applySourceRequest(&spec, sourceRequest{Name: "prod-db"}, false); msg != "" {
		t.Fatal(msg)
	}
	if spec.RestoreVerification != nil {
		t.Errorf("empty verification fields must not synthesise a block: %+v", spec.RestoreVerification)
	}
	if spec.Analyzer != nil || spec.Retention != nil {
		t.Errorf("empty optional fields must not synthesise blocks: analyzer=%+v retention=%+v", spec.Analyzer, spec.Retention)
	}
}

func TestApplySourceRequest_ClearByEmptyStringOnUpdate(t *testing.T) {
	// Given a spec that already has the full verification block, submitting
	// empty strings in the form must remove it. This is the "revert to
	// default" affordance the SPA exposes by clearing inputs.
	spec := v1alpha1.BackupSourceSpec{
		RestoreVerification: &v1alpha1.RestoreVerificationSpec{Mode: "stream-validate", Image: "postgres:15"},
		Retention:           &v1alpha1.RetentionSpec{Days: ptrInt32(7)},
		JitterMinutes:       ptrInt32(10),
	}
	if msg := applySourceRequest(&spec, sourceRequest{Name: "prod-db"}, true); msg != "" {
		t.Fatal(msg)
	}
	if spec.RestoreVerification != nil || spec.Retention != nil || spec.JitterMinutes != nil {
		t.Errorf("empty form values must clear optional blocks on update: %+v", spec)
	}
}

func TestApplySourceRequest_UpdatesExisting(t *testing.T) {
	spec := v1alpha1.BackupSourceSpec{
		RestoreVerification: &v1alpha1.RestoreVerificationSpec{Mode: "stream-validate"},
		Database:            v1alpha1.DatabaseSpec{Host: "old", Username: "u"},
	}
	msg := applySourceRequest(&spec, sourceRequest{
		Name:                    "prod-db",
		RestoreVerificationMode: "schema-only",
		VerificationImage:       "postgres:15.5",
		RowDropThreshold:        "0.3",
	}, true)
	if msg != "" {
		t.Fatal(msg)
	}
	if spec.RestoreVerification.Mode != "schema-only" || spec.RestoreVerification.Image != "postgres:15.5" {
		t.Errorf("verification not updated: %+v", spec.RestoreVerification)
	}
	if spec.Analyzer == nil || spec.Analyzer.RowDropThresholdPercent == nil || *spec.Analyzer.RowDropThresholdPercent != 30 {
		t.Errorf("ratio 0.3 must become 30 percent: %+v", spec.Analyzer)
	}
	if spec.Database.Host != "old" || spec.Database.Username != "u" {
		t.Errorf("empty identity fields must be preserved on update: %+v", spec.Database)
	}
}

func TestApplySourceRequest_RejectsBadValues(t *testing.T) {
	cases := []sourceRequest{
		{Port: "70000"},
		{Schedule: "99 * * * *"},
		{JitterMinutes: "61"},
		{RetentionDays: "-1"},
		{RowDropThreshold: "1.5"},
		{Compression: "brotli"},
		{RestoreVerificationMode: "stream"},
		{RestoreVerificationInterval: "weekly"},
		{VerificationVolumeSize: "lots"},
	}
	for i, req := range cases {
		var spec v1alpha1.BackupSourceSpec
		if msg := applySourceRequest(&spec, req, false); msg == "" {
			t.Errorf("case %d (%+v): expected a validation message", i, req)
		}
	}
}

func TestSourceInfoFrom_RoundTrip(t *testing.T) {
	var spec v1alpha1.BackupSourceSpec
	req := sourceRequest{
		Name: "prod-db", DBType: "postgres", Host: "db", Port: "5433", Database: "app", Username: "u",
		Schedule: "0 3 * * *", JitterMinutes: "0", RetentionDays: "14", MinKeep: "2",
		RowDropThreshold: "0.25", Compression: "zstd", RestoreVerificationMode: "full",
	}
	if msg := applySourceRequest(&spec, req, false); msg != "" {
		t.Fatal(msg)
	}
	bs := &v1alpha1.BackupSource{Spec: spec}
	bs.Name = "prod-db"
	info := sourceInfoFrom(bs, true)
	if info.Port != "5433" || info.JitterMinutes != "0" || info.RetentionDays != "14" || info.MinKeep != "2" ||
		info.RowDropThreshold != "0.25" || info.Compression != "zstd" || info.RestoreVerificationMode != "full" || !info.HasPassword {
		t.Errorf("round-trip mismatch: %+v", info)
	}
}

func TestApplyDestinationData_RoutesKeys(t *testing.T) {
	spec := v1alpha1.BackupDestinationSpec{Type: v1alpha1.StorageSFTP}
	creds := map[string][]byte{}
	msg := applyDestinationData(&spec, map[string]string{
		"host": "sb", "port": "23", "username": "u", "known-hosts": "kh", "insecure-skip-host-verify": "true",
		"ssh-private-key": "PEM", "password": "",
	}, nil, creds)
	if msg != "" {
		t.Fatal(msg)
	}
	if spec.SFTP == nil || spec.SFTP.Host != "sb" || spec.SFTP.Port == nil || *spec.SFTP.Port != 23 || !spec.SFTP.InsecureSkipHostVerify {
		t.Errorf("connection keys must land in the spec: %+v", spec.SFTP)
	}
	if string(creds["ssh-private-key"]) != "PEM" {
		t.Errorf("credential keys must land in creds: %v", creds)
	}
	if _, ok := creds["password"]; ok {
		t.Error("empty credential values must be skipped (masked field left blank on edit)")
	}
	// removeKeys clears a connection field; unknown keys are rejected.
	if msg := applyDestinationData(&spec, nil, []string{"known-hosts"}, creds); msg != "" || spec.SFTP.KnownHosts != "" {
		t.Errorf("removeKeys must clear known-hosts: msg=%q spec=%+v", msg, spec.SFTP)
	}
	if msg := applyDestinationData(&spec, map[string]string{"bucket": "x"}, nil, creds); msg == "" {
		t.Error("a key from another backend must be rejected")
	}
}

func TestFlattenDestinationSpec_RoundTrip(t *testing.T) {
	spec := v1alpha1.BackupDestinationSpec{Type: v1alpha1.StorageS3}
	if msg := applyDestinationData(&spec, map[string]string{"bucket": "b", "endpoint": "http://m", "path-style": "true", "access-key-id": "k"}, nil, map[string][]byte{}); msg != "" {
		t.Fatal(msg)
	}
	flat := flattenDestinationSpec(&spec)
	if flat["bucket"] != "b" || flat["endpoint"] != "http://m" || flat["path-style"] != "true" {
		t.Errorf("flatten mismatch: %v", flat)
	}
	if _, leaked := flat["access-key-id"]; leaked {
		t.Error("credentials never live in the spec")
	}
}

func TestExtractTimestamp(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"prod-db/2026/05/01/dump-20260501T020000Z.meta.json", "20260501T020000Z"},
		{"dump-20260501T020000Z.meta.json", "20260501T020000Z"},
		{"some/deep/path/dump-20260101T000000Z.meta.json", "20260101T000000Z"},
		{"not-a-meta.json", ""},
		{"", ""},
		{"dump-short.meta.json", ""},
	}
	for _, c := range cases {
		got := extractTimestamp(c.path)
		if got != c.want {
			t.Errorf("extractTimestamp(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}
