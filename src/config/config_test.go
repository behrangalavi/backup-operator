package config

import (
	"fmt"
	"testing"
)

func TestInitializeConfigModule_RequiredMissing(t *testing.T) {
	t.Setenv("TEST_MISSING_KEY", "")
	err := InitializeConfigModule([]ConfigItemDescription{
		{Key: "TEST_MISSING_KEY", Optional: false},
	})
	if err == nil {
		t.Error("expected error for missing required key")
	}
}

func TestInitializeConfigModule_OptionalMissing(t *testing.T) {
	t.Setenv("TEST_OPT_KEY", "")
	err := InitializeConfigModule([]ConfigItemDescription{
		{Key: "TEST_OPT_KEY", Optional: true, Default: "fallback"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := GetValue("TEST_OPT_KEY")
	if got != "fallback" {
		t.Errorf("GetValue = %q, want %q", got, "fallback")
	}
}

func TestInitializeConfigModule_EnvOverridesDefault(t *testing.T) {
	t.Setenv("TEST_OVERRIDE", "from-env")
	err := InitializeConfigModule([]ConfigItemDescription{
		{Key: "TEST_OVERRIDE", Optional: true, Default: "default-val"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := GetValue("TEST_OVERRIDE")
	if got != "from-env" {
		t.Errorf("GetValue = %q, want %q", got, "from-env")
	}
}

func TestInitializeConfigModule_ValidationFails(t *testing.T) {
	t.Setenv("TEST_VALIDATE", "bad")
	err := InitializeConfigModule([]ConfigItemDescription{
		{
			Key:      "TEST_VALIDATE",
			Optional: true,
			Default:  "bad",
			Validate: func(v string) error {
				if v == "bad" {
					return fmt.Errorf("value %q is not allowed", v)
				}
				return nil
			},
		},
	})
	if err == nil {
		t.Error("expected validation error")
	}
}

func TestInitializeConfigModule_ValidationPasses(t *testing.T) {
	t.Setenv("TEST_VALID", "good")
	err := InitializeConfigModule([]ConfigItemDescription{
		{
			Key:      "TEST_VALID",
			Optional: true,
			Default:  "good",
			Validate: func(v string) error {
				return nil
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := GetValue("TEST_VALID")
	if got != "good" {
		t.Errorf("GetValue = %q, want %q", got, "good")
	}
}
