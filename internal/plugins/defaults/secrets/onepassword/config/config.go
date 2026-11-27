package config

import (
	secretsconfig "github.com/nickheyer/icebath/internal/secrets/config"
	vaultconfig "github.com/nickheyer/icebath/internal/vault/config"
)

// Name of the store, of its section and of its subcommand
const Name = "onepassword"

var (
	Key        = secretsconfig.Section(Name)
	Enabled    = secretsconfig.Of(Name, secretsconfig.Enabled)
	Override   = secretsconfig.Of(Name, secretsconfig.Override)
	Timeout    = secretsconfig.Of(Name, secretsconfig.Timeout)
	TokenEnv   = Key + "." + TokenSetting
	References = Key + ".env"
	Account    = Key + ".account"
	Binary     = Key + ".binary_path"
	CacheTTL   = Key + ".cache_ttl_seconds"
)

// Setting naming the variable of a service account token, and that variable unless it names another
const (
	TokenSetting    = "service_account_token_env"
	DefaultTokenEnv = "OP_SERVICE_ACCOUNT_TOKEN"
)

// Where the 1Password command-line tool is installed from
const Docs = "https://developer.1password.com/docs/cli/get-started/"

// 1Password as a source of website logins for the vault
var (
	VaultKey      = vaultconfig.Key + "." + Name
	VaultEnabled  = VaultKey + ".enabled"
	VaultAccount  = VaultKey + ".account"
	VaultBinary   = VaultKey + ".binary_path"
	VaultTokenEnv = VaultKey + ".service_account_token_env"
)
