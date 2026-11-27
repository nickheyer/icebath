package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Hooks",
		About: "Your own commands that run when the agent acts",
		Group: config.GroupAbilities,
		Order: 80,
		Settings: []config.Setting{
			{Key: CallbackTimeout, Title: "Plugin time limit", About: "How long a hook of a plugin may take before it is cut off.", Default: 30 * time.Second,
				Zero: "No limit", Min: config.Bound(0), Max: config.Bound(10 * time.Minute), DefaultBelow: true, Advanced: true},
			{Key: StreamReasoning, Title: "Share reasoning", About: "Let hooks follow the reasoning of the model as it streams in.", Default: false,
				Advanced: true},
			{Key: PayloadMaxChars, Title: "Data limit", About: "The most text of each request and reply that a hook is handed.", Default: 50000,
				Counts: "characters", Least: config.Num(1000), Env: []string{"ICEBATH_PLUGIN_PAYLOAD_MAX_CHARS"}, Advanced: true},
		},
	}, {
		Key:    SpillKey,
		Parent: Key,
		Title:  "Long output",
		About:  "Where long text from a hook is saved and what is kept",
		Order:  20,
		Settings: []config.Setting{
			{Key: SpillEnabled, Title: "Save to a file", About: "Save long output from hooks and plugins to a file and keep a preview.", Default: true},
			{Key: SpillMaxChars, Title: "Size limit", About: "How long output may get before it is saved to a file.", Default: 10000,
				Zero: "10,000 characters", Counts: "characters", Advanced: true},
			{Key: SpillPreviewHead, Title: "Preview start", About: "How much of the start of saved output the preview keeps.", Default: 500,
				Counts: "characters", Least: config.Num(0), Advanced: true},
			{Key: SpillPreviewTail, Title: "Preview end", About: "How much of the end of saved output the preview keeps.", Default: 500,
				Counts: "characters", Least: config.Num(0), Advanced: true},
			{Key: SpillDirectory, Title: "Folder", About: "The folder that long output is saved in.", Help: "By default a folder named hook_outputs in the profile's folder.",
				Default: "", Zero: "Profile's hook_outputs", Advanced: true},
		},
	}})
}
