package httpclient

import (
	"os"
	"strings"
)

// Lookup of one environment variable reporting whether it is set
type envLookup func(key string) (string, bool)

// Lookup over a KEY=value list such as a child process environment, later entries winning
func envList(environ []string) envLookup {
	values := make(map[string]string, len(environ))
	for _, kv := range environ {
		if key, value, ok := strings.Cut(kv, "="); ok && key != "" {
			values[envKey(key)] = value
		}
	}
	return func(key string) (string, bool) {
		value, ok := values[envKey(key)]
		return value, ok
	}
}

// Value of a process variable with surrounding whitespace removed, empty when unset
func envValue(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

// List with every assignment of key replaced by one assignment of value at the end
func setEnv(environ []string, key, value string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if name, _, ok := strings.Cut(kv, "="); ok && envKey(name) == envKey(key) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, key+"="+value)
}
