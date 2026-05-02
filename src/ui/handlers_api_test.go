package ui

import "testing"

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
