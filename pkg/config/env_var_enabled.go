package config

// True when the named variable trimmed and lowercased is 1, true, yes or on
func EnvVarEnabled(name string) bool { return envVarEnabled(name) }
