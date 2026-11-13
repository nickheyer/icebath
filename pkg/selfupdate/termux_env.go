package selfupdate

import (
	"os"
	"strings"
)

// Reports a Termux environment from env, or from the process environment when env is empty
func IsTermuxEnv(env map[string]string) bool {
	get := os.Getenv
	if len(env) > 0 {
		get = func(key string) string { return env[key] }
	}
	prefix := get("PREFIX")
	return get("TERMUX_VERSION") != "" || strings.Contains(prefix, "com.termux/files/usr") || strings.HasPrefix(prefix, "/data/data/com.termux/")
}
