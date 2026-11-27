package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Autofill vault",
		About: "Logins, cards and addresses the agent may fill into web pages for you, kept encrypted in the profile or read from your password managers; the agent never sees a value",
		Settings: []config.Setting{
			{Key: KeyStorage, Title: "Vault key storage", About: "Where a new vault's encryption key is kept", Default: "file",
				Options: config.Options(
					config.Option{Value: "file", Title: "File", About: "A key file in the profile folder, beside the encrypted vault"},
					config.Option{Value: "keychain", Title: "OS keychain", About: "The system keychain"},
				)},
		},
	}})
}
