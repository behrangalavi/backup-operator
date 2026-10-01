package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"backup-operator/assert"
)

type ConfigItemDescription struct {
	Optional bool
	Default  string
	Key      string
	Validate func(value string) error
}

type ConfigItem struct {
	Key   string
	Value string
}

type configModule struct {
	settings map[string]ConfigItem
}

var (
	staticConfigModule *configModule
	configMu           sync.RWMutex
)

func InitializeConfigModule(configs []ConfigItemDescription) error {
	mod := &configModule{
		settings: make(map[string]ConfigItem, len(configs)),
	}

	for i := range configs {
		decl := configs[i]

		envVar := os.Getenv(decl.Key)
		if envVar == "" && !decl.Optional {
			return fmt.Errorf("option %s is not set", decl.Key)
		}
		value := decl.Default
		if envVar != "" {
			value = envVar
		}

		if decl.Validate != nil {
			if err := decl.Validate(value); err != nil {
				// Name the key: a bare "must be an integer" from a shared
				// validator is useless in a crash log with 40 config keys.
				return fmt.Errorf("config %s: %w", decl.Key, err)
			}
		}

		mod.settings[decl.Key] = ConfigItem{
			Key:   decl.Key,
			Value: value,
		}
	}

	configMu.Lock()
	staticConfigModule = mod
	configMu.Unlock()
	return nil
}

func GetValue(key string) string {
	configMu.RLock()
	mod := staticConfigModule
	configMu.RUnlock()
	assert.Assert(mod != nil, "static config module has never been initialized")
	return mod.settings[key].Value
}

// GetInt returns the config value parsed as int. A value that does not parse
// is a programming error — every integer key must carry a Validate at
// declaration time — so this panics rather than silently returning 0 (which
// for keys like DEFAULT_MIN_KEEP would quietly remove a safety floor).
func GetInt(key string) int {
	raw := GetValue(key)
	v, err := strconv.Atoi(raw)
	assert.NoError(err, fmt.Sprintf("config %s=%q is not an integer (missing Validate at declaration?)", key, raw))
	return v
}

// GetInt64 is GetInt for int64 values. Same panic-on-garbage contract.
func GetInt64(key string) int64 {
	raw := GetValue(key)
	v, err := strconv.ParseInt(raw, 10, 64)
	assert.NoError(err, fmt.Sprintf("config %s=%q is not an integer (missing Validate at declaration?)", key, raw))
	return v
}

// GetBool interprets common truthy spellings case-insensitively:
// true/1/yes/on. Everything else (including empty/unset) is false. This
// matters for security-relevant flags like UI_READ_ONLY — the previous
// exact "true"-only match meant UI_READ_ONLY=True or =1 silently left the
// read-only guard OFF with no error.
func GetBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(GetValue(key))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// GetDurationSeconds parses the config value as an integer number of seconds
// and returns it as a time.Duration.
func GetDurationSeconds(key string) time.Duration {
	return time.Duration(GetInt(key)) * time.Second
}
