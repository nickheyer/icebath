package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Hooks",
		About: "Plugins and commands that run on the agent's events: how long they may take and what they are handed",
		Settings: []config.Setting{
			{Key: CallbackTimeout, Title: "Plugin hook limit", About: "Seconds a plugin hook may take before it is cut off, at most 600; 0 for no limit", Default: 30 * time.Second,
				Min: config.Bound(0), Max: config.Bound(10 * time.Minute), DefaultBelow: true},
			{Key: StreamReasoning, Title: "Stream reasoning", About: "Let hooks watch the model's reasoning as it streams", Default: false},
			{Key: PayloadMaxChars, Title: "Hook payload limit", About: "Most characters of the request and response views a hook is handed, at least 1000",
				Default: 50000, Env: []string{"ICEBATH_PLUGIN_PAYLOAD_MAX_CHARS"}},
		},
	}, {
		Key:    SpillKey,
		Parent: Key,
		Title:  "Long hook output",
		About:  "Long text a hook adds to the turn is saved to a file and a preview of it kept",
		Settings: []config.Setting{
			{Key: SpillEnabled, Title: "Save long hook output", About: "Save long hook and plugin output to a file and keep a preview", Default: true},
			{Key: SpillMaxChars, Title: "Hook output limit", About: "Characters above which hook output is saved to a file", Default: 10000},
			{Key: SpillPreviewHead, Title: "Preview start", About: "Characters from the start of saved hook output kept in the preview", Default: 500},
			{Key: SpillPreviewTail, Title: "Preview end", About: "Characters from the end of saved hook output kept in the preview", Default: 500},
			{Key: SpillDirectory, Title: "Hook output folder", About: "Folder long hook output is saved in; empty uses hook_outputs in the profile's folder", Default: ""},
		},
	}})
}
