package app

import (
	"context"
	"os"
	"sort"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Persists a dotted key into the home config.yaml with owner only permissions, false when the write fails
func saveConfigValue(ctx context.Context, keyPath string, value any) bool {
	configPath := home.ConfigPath(home.Dir(ctx))
	if _, err := home.Mkdir(ctx, home.Dir(ctx)); err != nil {
		cliLogger.Error("Failed to save config: " + err.Error())
		return false
	}
	if err := config.WriteConfigKey(ctx, configPath, keyPath, value); err != nil {
		cliLogger.Error("Failed to save config: " + err.Error())
		return false
	}
	_ = os.Chmod(configPath, 0o600)
	return true
}

// Ordered JSON object of a plain map, nested maps converted with their keys sorted
func objectFromPlain(m map[string]any) *pyjson.Object {
	out := pyjson.NewObject()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := m[k]
		if nested, ok := v.(map[string]any); ok {
			v = objectFromPlain(nested)
		}
		out.Set(k, v)
	}
	return out
}
