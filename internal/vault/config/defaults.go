package config

import "github.com/nickheyer/icebath/internal/config"

// Registers the settings of the vault. Where its key is changes only by moving the key, which the page of the vault and icebath vault key
// move do, the setting following where the key is
func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Autofill vault",
		About: "Logins and cards the agent may fill into web pages",
		Group: config.GroupSecrets,
		Order: 20,
		Settings: []config.Setting{
			{Key: KeyStorage, Title: "Vault key", About: "Where the key that encrypts the vault is kept.", Default: "file",
				Options: config.Options(
					config.Option{Value: "file", Title: "A file", About: "A key file in the profile folder, beside the vault"},
					config.Option{Value: "keychain", Title: "The keychain", About: "The keychain of this computer"},
				), Custom: true},
		},
	}})
}
