package config

import (
	"fmt"
	"strconv"
)

// Defaults shared by the operator and the worker binaries. Both declare the
// same keys in their own config schema; keeping the literal values here means
// a change lands in both at once instead of drifting (3600 in one, 1800 in
// the other) without anyone noticing.
const (
	DefaultSchedule          = "0 2 * * *"
	DefaultRunTimeoutSeconds = "3600"
	DefaultRetentionDays     = "30"
	DefaultMinKeep           = "3"
	DefaultTempDir           = "/tmp/backup-operator"
)

// ValidatePositiveInt accepts integers > 0. Use for values that become
// timeouts or deadlines, where 0 or a negative number is nonsensical
// (RUN_TIMEOUT_SECONDS flows into activeDeadlineSeconds; <= 0 fails every Job
// immediately).
func ValidatePositiveInt(v string) error {
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("must be an integer: %w", err)
	}
	if n <= 0 {
		return fmt.Errorf("must be > 0, got %d", n)
	}
	return nil
}

// ValidateNonNegInt accepts integers >= 0. Use for counts where 0 is a
// legitimate "disabled" value (DEFAULT_RETENTION_DAYS=0 keeps forever,
// DEFAULT_MIN_KEEP=0 removes the safety floor).
func ValidateNonNegInt(v string) error {
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("must be an integer: %w", err)
	}
	if n < 0 {
		return fmt.Errorf("must be >= 0, got %d", n)
	}
	return nil
}

// ValidateIntRange accepts integers within [min, max].
func ValidateIntRange(min, max int) func(string) error {
	return func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("must be an integer: %w", err)
		}
		if n < min || n > max {
			return fmt.Errorf("must be between %d and %d, got %d", min, max, n)
		}
		return nil
	}
}
