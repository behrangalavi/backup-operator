package factory

import (
	"testing"

	"backup-operator/internal/labels"

	"github.com/go-logr/logr"
)

func TestNew_OffAndEmptyReturnNil(t *testing.T) {
	for _, mode := range []string{"", labels.RestoreVerificationOff} {
		v, err := New(mode, "postgres", logr.Discard())
		if err != nil {
			t.Errorf("mode %q: unexpected error %v", mode, err)
		}
		if v != nil {
			t.Errorf("mode %q: expected nil verifier so callers can early-out, got %T", mode, v)
		}
	}
}

func TestNew_ActiveModesReturnVerifier(t *testing.T) {
	cases := []struct {
		mode   string
		dbType string
	}{
		{labels.RestoreVerificationStreamValidate, "postgres"},
		{labels.RestoreVerificationSchemaOnly, "postgres"},
		{labels.RestoreVerificationSample, "mysql"},
		{labels.RestoreVerificationFull, "mongo"},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			v, err := New(c.mode, c.dbType, logr.Discard())
			if err != nil {
				t.Fatalf("mode %q: unexpected error %v", c.mode, err)
			}
			if v == nil {
				t.Errorf("mode %q: expected a non-nil verifier", c.mode)
			}
		})
	}
}

func TestNew_UnknownModeErrors(t *testing.T) {
	v, err := New("lz4-not-a-mode", "postgres", logr.Discard())
	if err == nil {
		t.Fatal("unknown mode must return an error")
	}
	if v != nil {
		t.Errorf("unknown mode must return nil verifier, got %T", v)
	}
}

// A Phase-2 mode with an unsupported db-type must propagate NewEngine's
// error rather than silently returning a half-built verifier.
func TestNew_Phase2UnsupportedDBTypeErrors(t *testing.T) {
	v, err := New(labels.RestoreVerificationSchemaOnly, "cassandra", logr.Discard())
	if err == nil {
		t.Fatal("unsupported db-type for a phase-2 mode must return an error")
	}
	if v != nil {
		t.Errorf("expected nil verifier on engine error, got %T", v)
	}
}
