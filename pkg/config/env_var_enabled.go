package config

import "os"

// True when the named variable trimmed and lowercased is 1, true, yes or on
func EnvVarEnabled(name string) bool { return isTruthyString(os.Getenv(name)) }
