package backup

import (
	"testing"

	"backup-operator/internal/labels"
	"backup-operator/internal/secrets"
)

func TestRestoreVerificationActive(t *testing.T) {
	cases := []struct {
		mode string
		want bool
	}{
		{"", false},
		{labels.RestoreVerificationOff, false},
		{labels.RestoreVerificationStreamValidate, true},
		{"full", true},
		{"schema-only", true},
	}
	for _, c := range cases {
		got := restoreVerificationActive(&secrets.Source{RestoreVerificationMode: c.mode})
		if got != c.want {
			t.Errorf("restoreVerificationActive(mode=%q) = %v, want %v", c.mode, got, c.want)
		}
	}
}
