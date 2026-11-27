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
			Title:  "Bitwarden",
			About:  "Bitwarden Secrets Manager: every secret of one project set as a variable as icebath starts, read through the bws program",
			Settings: []config.Setting{
				{Key: Enabled, Title: "Bitwarden secrets", About: "Load secrets from Bitwarden Secrets Manager at startup", Default: false},
				{Key: TokenEnv, Title: "Bitwarden token variable", About: "Environment variable holding the machine account's access token", Default: DefaultTokenEnv},
				{Key: ProjectID, Title: "Bitwarden project", About: "Bitwarden project to load secrets from; icebath secrets bitwarden setup picks one", Default: ""},
				{Key: ServerURL, Title: "Bitwarden server", About: "Address of another Bitwarden region or of your own server; empty uses the US cloud", Default: ""},
				{Key: CacheTTL, Title: "Bitwarden cache time", About: "Seconds fetched Bitwarden secrets are reused for; zero fetches them every time", Default: 5 * time.Minute},
				{Key: Override, Title: "Bitwarden wins", About: "Bitwarden values replace matching values your shell or the .env set", Default: true},
				{Key: AutoInstall, Title: "Auto-install Bitwarden", About: "Download the pinned bws program when it is missing", Default: true},
				{Key: Timeout, Title: "Bitwarden time limit", About: "Seconds one fetch may take before icebath starts without it", Default: 2 * time.Minute},
			},
		},
		{
			Key:    Encrypted,
			Parent: Key,
			Title:  "Offline cache",
			About:  "An encrypted copy of the last secrets fetched, used when Bitwarden can't be reached",
			Settings: []config.Setting{
				{Key: EncryptedOn, Title: "Offline Bitwarden cache", About: "Keep an encrypted copy to use when Bitwarden is unreachable", Default: false},
				{Key: EncryptedStale, Title: "Offline cache age", About: "Oldest offline copy that may still be used, in seconds; zero never uses it", Default: time.Duration(0)},
			},
		},
		{
			Key:    VaultKey,
			Parent: vaultconfig.Key,
			Title:  "Bitwarden",
			About:  "Bitwarden Password Manager as a source of website logins, read through the bw program once you unlock it",
			Settings: []config.Setting{
				{Key: VaultEnabled, Title: "Bitwarden logins", About: "Offer Bitwarden as a source of website logins when its bw program is installed", Default: true},
				{Key: VaultBinary, Title: "Bitwarden tool path", About: "Full path of the bw program; empty looks it up on the search path", Default: ""},
			},
		},
	})
}
