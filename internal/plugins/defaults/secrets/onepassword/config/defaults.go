package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
	secretsconfig "github.com/nickheyer/icebath/internal/secrets/config"
	vaultconfig "github.com/nickheyer/icebath/internal/vault/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{
		{
			Key:    Key,
			Parent: secretsconfig.Key,
			Title:  "1Password",
			About:  "1Password: variables that take their values from op:// references, read through the op program as icebath starts",
			Settings: []config.Setting{
				{Key: Enabled, Title: "1Password secrets", About: "Load secrets from 1Password at startup", Default: false},
				{Key: References, Title: "1Password references", About: "Variables to load, as name and op:// reference", Default: map[string]any{}},
				{Key: Account, Title: "1Password account", About: "1Password account to use, its shorthand or sign in address; empty uses the default account", Default: ""},
				{Key: TokenEnv, Title: "1Password token variable", About: "Environment variable holding the service account token; with none set, the session op is signed in to is used",
					Default: DefaultTokenEnv},
				{Key: Binary, Title: "1Password tool path", About: "Full path of the op program; empty looks it up on the search path", Default: ""},
				{Key: CacheTTL, Title: "1Password cache time", About: "Seconds resolved 1Password values are reused for; zero resolves them every time", Default: 5 * time.Minute},
				{Key: Override, Title: "1Password wins", About: "1Password values replace matching values your shell or the .env set", Default: true},
				{Key: Timeout, Title: "1Password time limit", About: "Seconds one resolution of every reference may take before icebath starts without it", Default: 2 * time.Minute},
			},
		},
		{
			Key:    VaultKey,
			Parent: vaultconfig.Key,
			Title:  "1Password",
			About:  "1Password as a source of website logins, read through the op program once you unlock it or with a service account token",
			Settings: []config.Setting{
				{Key: VaultEnabled, Title: "1Password logins", About: "Offer 1Password as a source of website logins when its op program is installed", Default: true},
				{Key: VaultAccount, Title: "1Password account", About: "1Password account to use for website logins; empty uses the default account", Default: ""},
				{Key: VaultBinary, Title: "1Password tool path", About: "Full path of the op program; empty looks it up on the search path", Default: ""},
				{Key: VaultTokenEnv, Title: "1Password token variable", About: "Environment variable holding the service account token, which skips the unlock", Default: DefaultTokenEnv},
			},
		},
	})
}
