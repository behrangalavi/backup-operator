package config

import (
	"fmt"
	"os"

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

var staticConfigModule *configModule

func InitializeConfigModule(configs []ConfigItemDescription) error {
	staticConfigModule = &configModule{
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

		staticConfigModule.settings[decl.Key] = ConfigItem{
			Key:   decl.Key,
			Value: value,
		}
	}
	return nil
}

func GetValue(key string) string {
	assert.Assert(staticConfigModule != nil, "static config module has never been initialized")
	return staticConfigModule.settings[key].Value
}
