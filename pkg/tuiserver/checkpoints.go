package tuiserver

import (
	"os"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Whether TUI created agents checkpoint the filesystem, the legacy bool form and the shipped default included
func loadCheckpointsEnabled(cfg map[string]any) bool {
	var section map[string]any
	switch raw := cfg["checkpoints"].(type) {
	case bool:
		section = map[string]any{"enabled": raw}
	case map[string]any:
		section = raw
	default:
		section = map[string]any{}
	}
	enabled, present := section["enabled"]
	if !present {
		defaults, _ := config.Defaults()["checkpoints"].(map[string]any)
		enabled = defaults["enabled"]
	}
	return truthy(enabled)
}

// Checkpoint enablement, the ICEBATH_TUI_CHECKPOINTS opt in first and then the config section
func resolveCheckpointsEnabled(cfg map[string]any) bool {
	if value, set := os.LookupEnv("ICEBATH_TUI_CHECKPOINTS"); set {
		return provider.IsTruthyValue(value)
	}
	return loadCheckpointsEnabled(cfg)
}
