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
				return err
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

// GetInt returns the config value parsed as int. Returns 0 on parse error.
func GetInt(key string) int {
	v, _ := strconv.Atoi(GetValue(key))
	return v
}

// GetInt64 returns the config value parsed as int64. Returns 0 on parse error.
func GetInt64(key string) int64 {
	v, _ := strconv.ParseInt(GetValue(key), 10, 64)
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
