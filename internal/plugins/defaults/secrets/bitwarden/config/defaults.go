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
			About:  "Load API keys from Bitwarden Secrets Manager",
			Order:  20,
			Settings: []config.Setting{
				{Key: Enabled, Title: "Load from Bitwarden", About: "Load API keys and other variables from Bitwarden Secrets Manager.", Default: false,
					Custom: true},
				{Key: ProjectID, Title: "Project", About: "The Bitwarden project whose secrets are loaded.", Default: "", Custom: true},
				{Key: Override, Title: "Replace your values", About: "Replace values that your shell or keys file already set.", Default: true},
				{Key: TokenEnv, Title: "Token variable", About: "The environment variable that holds the access token of the machine account.",
					Default: DefaultTokenEnv, Advanced: true},
				{Key: ServerURL, Title: "Server address", About: "The address of another Bitwarden region or of your own server.", Default: "", Zero: "The US cloud",
					Starts: []string{"https://", "http://"}, Custom: true},
				{Key: AutoInstall, Title: "Install bws", About: "Download the bws program when it is missing.", Default: true, Advanced: true},
				{Key: CacheTTL, Title: "Reuse values for", About: "How long fetched values are reused before Bitwarden is asked again.", Default: 5 * time.Minute,
					Zero: "Not reused", Advanced: true},
				{Key: Timeout, Title: "Time limit", About: "How long one fetch may take before icebath goes on without it.", Default: 2 * time.Minute,
					Zero: "2 minutes", Advanced: true},
			},
		},
		{
			Key:    Encrypted,
			Parent: Key,
			Title:  EncryptedTitle,
			About:  "An encrypted copy for when Bitwarden can't be reached",
			Order:  10,
			Settings: []config.Setting{
				{Key: EncryptedOn, Title: "Keep an offline copy", About: "Keep an encrypted copy of your secrets for when Bitwarden can't be reached.",
					Default: false},
				{Key: EncryptedStale, Title: "Oldest copy used", About: "How old the offline copy may be and still be used.", Default: time.Duration(0),
					Zero: "Never used"},
			},
		},
		{
			Key:    VaultKey,
			Parent: vaultconfig.Key,
			Title:  "Bitwarden",
			About:  "Website logins from your Bitwarden vault",
			Order:  20,
			Settings: []config.Setting{
				{Key: VaultEnabled, Title: "Use Bitwarden logins", About: "Offer logins from Bitwarden when its bw program is installed.", Default: true,
					Custom: true},
				{Key: VaultBinary, Title: "bw program", About: "The bw program that icebath runs.", Default: "", Zero: "Found on PATH", Advanced: true,
					Path: &config.Path{Kind: config.PathProgram, Exists: true}},
			},
		},
	})
}
