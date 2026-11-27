package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Secret stores",
		About: "Load API keys from Bitwarden, 1Password or a command",
		Group: config.GroupSecrets,
		Order: 10,
		Settings: []config.Setting{
			{Key: Sources, Title: "Store order", About: "The stores asked first, in order, by name.",
				Help: "The first store to give a variable wins it. Stores not named follow in their own order.", Default: []string{}, Zero: "Their own order",
				Apply: config.Restart},
			{Key: Preserve, Title: "Keep your values", About: "Variables no store may replace once your shell or keys file sets them.", Default: []string{},
				Apply: config.Restart},
			{Key: ProfileAlias, Title: "Profile aliases", About: "Set a secret named for the profile in use under its plain name too.",
				Help: "In profile work, OPENAI_API_KEY_WORK also sets OPENAI_API_KEY when nothing else does.", Default: true, Apply: config.Restart},
		},
	}})
}
