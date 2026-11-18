package config

import (
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
)

// Env name of the home .env key under the icebath prefix, false for a key that never carried the old prefix
func renamedEnvKey(key string) (string, bool) {
	for _, pair := range [][2]string{{"HERMES_", "ICEBATH_"}, {"_HERMES_", "_ICEBATH_"}} {
		if rest, ok := strings.CutPrefix(key, pair[0]); ok {
			return pair[1] + rest, true
		}
	}
	return "", false
}

// Renames every old prefixed key of the home .env to its ICEBATH_ name, a value already set under the new name winning
func migrateTo63(r *migrationRun) error {
	if !r.localFilesAllowed() {
		return nil
	}
	env := LoadEnvFile(home.EnvPath(r.h))
	var old []string
	for key := range env {
		if _, ok := renamedEnvKey(key); ok {
			old = append(old, key)
		}
	}
	sort.Strings(old)
	for _, key := range old {
		renamed, _ := renamedEnvKey(key)
		if _, taken := env[renamed]; taken {
			if _, err := r.src.RemoveEnvValue(key); err != nil {
				return err
			}
			r.say("  ✓ Removed " + key + " from .env, " + renamed + " is already set there")
			continue
		}
		if err := r.src.SaveEnvValue(renamed, env[key]); err != nil {
			return err
		}
		if _, err := r.src.RemoveEnvValue(key); err != nil {
			return err
		}
		r.res.EnvAdded = append(r.res.EnvAdded, renamed)
		r.say("  ✓ Renamed " + key + " to " + renamed + " in .env")
	}
	return nil
}
