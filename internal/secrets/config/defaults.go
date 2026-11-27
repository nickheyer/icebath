package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Secret stores",
		About: "Stores API keys and other variables come from as icebath starts, such as Bitwarden and 1Password, instead of the profile's .env",
		Settings: []config.Setting{
			{Key: Sources, Title: "Store order", About: "Secret stores asked first, by name; the first store to supply a variable wins it. Stores not named follow in their own order",
				Default: []string{}},
			{Key: Preserve, Title: "Keep local values", About: "Variables no secret store may overwrite once your shell or the profile's .env sets them",
				Default: []string{}},
			{Key: ProfileAlias, Title: "Profile name aliases", About: "A secret named for the profile in use, such as OPENAI_API_KEY_WORK in profile work, is also set as OPENAI_API_KEY when nothing else supplies that",
				Default: true},
		},
	}})
}
