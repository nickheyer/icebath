package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(Providers, map[string]any{})
}

func Entries(cfg *config.Config) map[string]map[string]any {
	entries := map[string]map[string]any{}
	configured, _ := cfg.Get(Providers).(map[string]any)
	for name, value := range configured {
		if entry, ok := value.(map[string]any); ok {
			entries[name] = entry
		}
	}
	return entries
}

func Field(entry map[string]any, field string) string {
	value, _ := entry[field].(string)
	return value
}
