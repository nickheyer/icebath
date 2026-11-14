//go:build windows

package config

import (
	"os"
	"strings"
)

// Name env policy comparisons use, upper case because Windows variable names ignore case
func envPolicyName(key string) string { return upperText(key) }

// Process variable names in upper case in the order the environment lists them, with their values by name
func processEnvList() ([]string, map[string]string) {
	var names []string
	values := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		name := envPolicyName(k)
		if _, dup := values[name]; ok && k != "" && !dup {
			names = append(names, name)
			values[name] = v
		}
	}
	return names, values
}

// Process environment by name, names in upper case as Windows resolves them
func processEnviron() map[string]string {
	_, values := processEnvList()
	return values
}

// Sets a process variable under its upper case name
func setProcessEnv(name, value string) error { return os.Setenv(envPolicyName(name), value) }
