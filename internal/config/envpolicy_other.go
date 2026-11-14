//go:build !windows

package config

import (
	"os"
	"strings"
)

// Name env policy comparisons use, the name itself because POSIX variable names keep their case
func envPolicyName(key string) string { return key }

// Process variable names in the order the environment lists them, with their values by name
func processEnvList() ([]string, map[string]string) {
	var names []string
	values := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if _, dup := values[k]; ok && !dup {
			names = append(names, k)
			values[k] = v
		}
	}
	return names, values
}

// Process environment by name
func processEnviron() map[string]string {
	_, values := processEnvList()
	return values
}

// Sets a process variable
func setProcessEnv(name, value string) error { return os.Setenv(name, value) }
