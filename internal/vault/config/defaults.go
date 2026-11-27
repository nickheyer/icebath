package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Autofill vault",
		About: "Logins and cards the agent may fill into web pages",
		Group: config.GroupSecrets,
		Order: 20,
		Settings: []config.Setting{
			{Key: KeyStorage, Title: "Key storage", About: "Where the encryption key of a new vault is kept.", Default: "file",
				Options: config.Options(
					config.Option{Value: "file", Title: "File", About: "A key file in the profile folder, beside the vault"},
					config.Option{Value: "keychain", Title: "System keychain", About: "The keychain of this computer"},
				), Advanced: true},
		},
	}})
}
