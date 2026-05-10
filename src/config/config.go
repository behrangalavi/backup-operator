package config

import (
	"fmt"
	"os"
	"sync"

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
