package config

import (
	secretsconfig "github.com/nickheyer/icebath/internal/secrets/config"
	vaultconfig "github.com/nickheyer/icebath/internal/vault/config"
)

// Name of the store, of its section and of its subcommand
const Name = "bitwarden"

var (
	Key            = secretsconfig.Section(Name)
	Enabled        = secretsconfig.Of(Name, secretsconfig.Enabled)
	Override       = secretsconfig.Of(Name, secretsconfig.Override)
	Timeout        = secretsconfig.Of(Name, secretsconfig.Timeout)
	TokenEnv       = Key + "." + TokenSetting
	AutoInstall    = Key + ".auto_install"
	CacheTTL       = Key + ".cache_ttl_seconds"
	ProjectID      = Key + ".project_id"
	ServerURL      = Key + ".server_url"
	Encrypted      = Key + ".encrypted_cache"
	EncryptedOn    = Encrypted + ".enabled"
	EncryptedStale = Encrypted + ".max_stale_seconds"
)

// Title of the encrypted copy of the secrets, its section and its page
const EncryptedTitle = "Offline copy"

// Setting naming the variable of the access token, and that variable unless it names another
const (
	TokenSetting    = "access_token_env"
	DefaultTokenEnv = "BWS_ACCESS_TOKEN"
)

// Variable naming another Bitwarden region or a server of the user's own, which bws reads
const ServerURLEnv = "BWS_SERVER_URL"

// Bitwarden Password Manager as a source of website logins for the vault
var (
	VaultKey     = vaultconfig.Key + "." + Name
	VaultEnabled = VaultKey + ".enabled"
	VaultBinary  = VaultKey + ".binary_path"
)
