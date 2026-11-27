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
			About:  "Load API keys from Bitwarden Secrets Manager at startup",
			Order:  20,
			Settings: []config.Setting{
				{Key: Enabled, Title: "Load from Bitwarden", About: "Load API keys and other variables from Bitwarden Secrets Manager at startup.",
					Default: false, Apply: config.Restart},
				{Key: ProjectID, Title: "Project", About: "The Bitwarden project whose secrets are loaded.", Help: "icebath secrets bitwarden setup picks one for you.",
					Default: "", Apply: config.Restart},
				{Key: Override, Title: "Replace your values", About: "Replace values that your shell or keys file already set.", Default: true,
					Apply: config.Restart},
				{Key: TokenEnv, Title: "Token variable", About: "The environment variable that holds the access token of the machine account.",
					Default: DefaultTokenEnv, Apply: config.Restart, Advanced: true},
				{Key: ServerURL, Title: "Server address", About: "The address of another Bitwarden region or of your own server.", Default: "", Zero: "The US cloud",
					Apply: config.Restart, Advanced: true},
				{Key: AutoInstall, Title: "Install bws", About: "Download the bws program when it is missing.", Default: true, Apply: config.Restart, Advanced: true},
				{Key: CacheTTL, Title: "Reuse values for", About: "How long fetched values are reused before Bitwarden is asked again.", Default: 5 * time.Minute,
					Zero: "Not reused", Apply: config.Restart, Advanced: true},
				{Key: Timeout, Title: "Time limit", About: "How long one fetch may take before icebath starts without it.", Default: 2 * time.Minute,
					Zero: "2 minutes", Apply: config.Restart, Advanced: true},
			},
		},
		{
			Key:    Encrypted,
			Parent: Key,
			Title:  "Offline copy",
			About:  "An encrypted copy for when Bitwarden can't be reached",
			Order:  10,
			Settings: []config.Setting{
				{Key: EncryptedOn, Title: "Keep an offline copy", About: "Keep an encrypted copy of your secrets for when Bitwarden can't be reached.",
					Default: false, Apply: config.Restart},
				{Key: EncryptedStale, Title: "Oldest copy used", About: "How old the offline copy may be and still be used.", Default: time.Duration(0),
					Zero: "Never used", Apply: config.Restart},
			},
		},
		{
			Key:    VaultKey,
			Parent: vaultconfig.Key,
			Title:  "Bitwarden",
			About:  "Website logins from your Bitwarden vault",
			Order:  20,
			Settings: []config.Setting{
				{Key: VaultEnabled, Title: "Use Bitwarden logins", About: "Offer logins from Bitwarden when its bw program is installed.", Default: true},
				{Key: VaultBinary, Title: "bw program", About: "The full path of the bw program.", Default: "", Zero: "Found on PATH", Advanced: true},
			},
		},
	})
}
