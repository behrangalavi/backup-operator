// Package factory wires the restore-verifier mode string to a concrete
// Verifier implementation. New modes register here; calling code branches
// on the mode string only inside this file (matches the dumper / storage
// factory pattern documented in CLAUDE.md §8.2).
package factory

import (
	"fmt"

	"backup-operator/internal/labels"
	"backup-operator/verifier"
	"backup-operator/verifier/restore"
	"backup-operator/verifier/stream"

	"github.com/go-logr/logr"
)

// New returns the Verifier matching mode + dbType. Mode "off" or empty
// returns (nil, nil) so callers can early-out without a special case.
func New(mode, dbType string, log logr.Logger) (verifier.Verifier, error) {
	switch mode {
	case "", labels.RestoreVerificationOff:
		return nil, nil
	case labels.RestoreVerificationStreamValidate:
		return stream.New(dbType, log), nil
	case labels.RestoreVerificationSchemaOnly:
		return restore.New(restore.ModeSchemaOnly, dbType, log)
	case labels.RestoreVerificationSample:
		return restore.New(restore.ModeSample, dbType, log)
	case labels.RestoreVerificationFull:
		return restore.New(restore.ModeFull, dbType, log)
	}
	return nil, fmt.Errorf("unknown restore-verification mode %q", mode)
}
