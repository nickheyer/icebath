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
			About:  "Load API keys from 1Password",
			Order:  10,
			Settings: []config.Setting{
				{Key: Enabled, Title: "Load from 1Password", About: "Load API keys and other variables from 1Password.", Default: false, Custom: true},
				{Key: References, Title: "Variables", About: "The variables 1Password fills, each with the reference that holds its value.",
					Default: map[string]any{}, Custom: true, Entry: &config.Entry{Noun: "variable", Name: "Variable", Fields: []config.Setting{
						{Title: "Reference", About: "Where 1Password keeps the value.", Help: "Such as op://Private/OpenAI/credential.", Default: "",
							Starts: []string{"op://"}},
					}}},
				{Key: Account, Title: "Account", About: "The 1Password account that op signs in to.", Default: "",
					Zero: "The account op uses by default", Picks: Accounts, Custom: true},
				{Key: Override, Title: "Replace your values", About: "Replace values that your shell or keys file already set.", Default: true},
				{Key: TokenEnv, Title: "Token variable", About: "The environment variable that holds a service account token.",
					Help: "With no token in it, icebath uses the account the op program is signed in to.", Default: DefaultTokenEnv, Advanced: true},
				{Key: Binary, Title: "op program", About: "The op program that icebath runs.", Default: "", Zero: "Found on PATH", Advanced: true,
					Path: &config.Path{Kind: config.PathProgram, Exists: true}},
				{Key: CacheTTL, Title: "Reuse values for", About: "How long loaded values are reused before 1Password is asked again.", Default: 5 * time.Minute,
					Zero: "Not reused", Advanced: true},
				{Key: Timeout, Title: "Time limit", About: "How long loading may take before icebath goes on without it.", Default: 2 * time.Minute,
					Zero: "2 minutes", Advanced: true},
			},
		},
		{
			Key:    VaultKey,
			Parent: vaultconfig.Key,
			Title:  "1Password",
			About:  "Website logins from your 1Password vault",
			Order:  10,
			Settings: []config.Setting{
				{Key: VaultEnabled, Title: "Use 1Password logins", About: "Offer logins from 1Password when its op program is installed.", Default: true,
					Custom: true},
				{Key: VaultAccount, Title: "Account", About: "The 1Password account that logins come from.", Default: "",
					Zero: "The account op uses by default", Picks: Accounts},
				{Key: VaultTokenEnv, Title: "Token variable", About: "The environment variable that holds a service account token.",
					Help: "A token lets icebath read logins without unlocking 1Password.", Default: DefaultTokenEnv, Advanced: true},
				{Key: VaultBinary, Title: "op program", About: "The op program that icebath runs.", Default: "", Zero: "Found on PATH", Advanced: true,
					Path: &config.Path{Kind: config.PathProgram, Exists: true}},
			},
		},
	})
}
