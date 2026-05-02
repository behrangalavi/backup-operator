package ui

import "testing"

func TestValidateSettings(t *testing.T) {
	cases := []struct {
		name string
		s    settingsPayload
		ok   bool
	}{
		{
			name: "valid",
			s:    settingsPayload{DefaultSchedule: "0 2 * * *", RunTimeoutSeconds: "3600"},
			ok:   true,
		},
		{
			name: "empty schedule",
			s:    settingsPayload{DefaultSchedule: ""},
			ok:   false,
		},
		{
			name: "invalid cron schedule (4 fields)",
			s:    settingsPayload{DefaultSchedule: "0 2 * *"},
			ok:   false,
		},
		{
			name: "timeout zero",
			s:    settingsPayload{DefaultSchedule: "0 2 * * *", RunTimeoutSeconds: "0"},
			ok:   false,
		},
		{
			name: "timeout negative",
			s:    settingsPayload{DefaultSchedule: "0 2 * * *", RunTimeoutSeconds: "-5"},
			ok:   false,
		},
		{
			name: "timeout valid",
			s:    settingsPayload{DefaultSchedule: "0 2 * * *", RunTimeoutSeconds: "1"},
			ok:   true,
		},
		{
			name: "retention negative",
			s:    settingsPayload{DefaultSchedule: "0 2 * * *", DefaultRetentionDays: "-1"},
			ok:   false,
		},
		{
			name: "minkeep negative",
			s:    settingsPayload{DefaultSchedule: "0 2 * * *", DefaultMinKeep: "-1"},
			ok:   false,
		},
	}
	for _, c := range cases {
		err := validateSettings(c.s)
		if c.ok && err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s: expected error", c.name)
		}
	}
}
