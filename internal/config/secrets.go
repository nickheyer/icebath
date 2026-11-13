package config

import "gopkg.in/yaml.v3"

// Browser credential vault section
type VaultConfig struct {
	KeyStorage  VaultKeyStorage  `yaml:"key_storage"`
	OnePassword VaultOnePassword `yaml:"onepassword"`
	Bitwarden   VaultBitwarden   `yaml:"bitwarden"`
	rest        rest
}

type vaultFields VaultConfig

// Decodes the mapping and keeps unknown keys
func (v *VaultConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*vaultFields)(v), &v.rest)
}

// Writes the mapping with unknown keys after the named ones
func (v VaultConfig) MarshalYAML() (any, error) {
	return encodeMapping(vaultFields(v), v.rest)
}

// Where a new vault keeps its key
type VaultKeyStorage string

// Vault key storages
const (
	VaultKeyFile     VaultKeyStorage = "file"
	VaultKeyKeychain VaultKeyStorage = "keychain"
)

// Accepted vault key storages
func VaultKeyStorages() []VaultKeyStorage {
	return []VaultKeyStorage{VaultKeyFile, VaultKeyKeychain}
}

// 1Password login source of the vault
type VaultOnePassword struct {
	Enabled                bool   `yaml:"enabled" coerce:"exact"`
	Account                string `yaml:"account"`
	BinaryPath             string `yaml:"binary_path"`
	ServiceAccountTokenEnv string `yaml:"service_account_token_env"`
	rest                   rest
}

type vaultOnePasswordFields VaultOnePassword

// Decodes the mapping and keeps unknown keys
func (v *VaultOnePassword) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*vaultOnePasswordFields)(v), &v.rest)
}

// Writes the mapping with unknown keys after the named ones
func (v VaultOnePassword) MarshalYAML() (any, error) {
	return encodeMapping(vaultOnePasswordFields(v), v.rest)
}

// Bitwarden login source of the vault
type VaultBitwarden struct {
	Enabled    bool   `yaml:"enabled" coerce:"exact"`
	BinaryPath string `yaml:"binary_path"`
	rest       rest
}

type vaultBitwardenFields VaultBitwarden

// Decodes the mapping and keeps unknown keys
func (v *VaultBitwarden) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*vaultBitwardenFields)(v), &v.rest)
}

// Writes the mapping with unknown keys after the named ones
func (v VaultBitwarden) MarshalYAML() (any, error) {
	return encodeMapping(vaultBitwardenFields(v), v.rest)
}

// External secret source section
type SecretsConfig struct {
	Bitwarden        SecretsBitwarden   `yaml:"bitwarden"`
	OnePassword      SecretsOnePassword `yaml:"onepassword"`
	Sources          []string           `yaml:"sources,omitempty"`
	PreserveExisting []string           `yaml:"preserve_existing,omitempty"`
	ProfileAlias     *bool              `yaml:"profile_alias,omitempty" coerce:"bool"`
	Command          *SecretsCommand    `yaml:"command,omitempty"`
	rest             rest
}

type secretsFields SecretsConfig

// Decodes the mapping and keeps unknown keys
func (s *SecretsConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*secretsFields)(s), &s.rest)
}

// Writes the mapping with unknown keys after the named ones
func (s SecretsConfig) MarshalYAML() (any, error) {
	return encodeMapping(secretsFields(s), s.rest)
}

// Bitwarden Secrets Manager source
type SecretsBitwarden struct {
	Enabled          bool           `yaml:"enabled" coerce:"bool"`
	AccessTokenEnv   string         `yaml:"access_token_env"`
	ProjectID        string         `yaml:"project_id"`
	CacheTTLSeconds  float64        `yaml:"cache_ttl_seconds"`
	EncryptedCache   EncryptedCache `yaml:"encrypted_cache"`
	OverrideExisting bool           `yaml:"override_existing" coerce:"bool"`
	AutoInstall      bool           `yaml:"auto_install" coerce:"bool"`
	ServerURL        string         `yaml:"server_url"`
	TimeoutSeconds   *float64       `yaml:"timeout_seconds,omitempty"`
	rest             rest
}

type secretsBitwardenFields SecretsBitwarden

// Decodes the mapping and keeps unknown keys
func (s *SecretsBitwarden) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*secretsBitwardenFields)(s), &s.rest)
}

// Writes the mapping with unknown keys after the named ones
func (s SecretsBitwarden) MarshalYAML() (any, error) {
	return encodeMapping(secretsBitwardenFields(s), s.rest)
}

// Last-good encrypted cache for network outages
type EncryptedCache struct {
	Enabled         bool    `yaml:"enabled" coerce:"bool"`
	MaxStaleSeconds float64 `yaml:"max_stale_seconds"`
	rest            rest
}

type encryptedCacheFields EncryptedCache

// Decodes the mapping and keeps unknown keys
func (e *EncryptedCache) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*encryptedCacheFields)(e), &e.rest)
}

// Writes the mapping with unknown keys after the named ones
func (e EncryptedCache) MarshalYAML() (any, error) {
	return encodeMapping(encryptedCacheFields(e), e.rest)
}

// 1Password secret source
type SecretsOnePassword struct {
	Enabled                bool        `yaml:"enabled" coerce:"bool"`
	Env                    Map[string] `yaml:"env"`
	Account                string      `yaml:"account"`
	ServiceAccountTokenEnv string      `yaml:"service_account_token_env"`
	BinaryPath             string      `yaml:"binary_path"`
	CacheTTLSeconds        float64     `yaml:"cache_ttl_seconds"`
	OverrideExisting       bool        `yaml:"override_existing" coerce:"bool"`
	TimeoutSeconds         *float64    `yaml:"timeout_seconds,omitempty"`
	rest                   rest
}

type secretsOnePasswordFields SecretsOnePassword

// Decodes the mapping and keeps unknown keys
func (s *SecretsOnePassword) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*secretsOnePasswordFields)(s), &s.rest)
}

// Writes the mapping with unknown keys after the named ones
func (s SecretsOnePassword) MarshalYAML() (any, error) {
	return encodeMapping(secretsOnePasswordFields(s), s.rest)
}

// Helper command secret source printing a KEY=VALUE blob
type SecretsCommand struct {
	Enabled              bool     `yaml:"enabled" coerce:"bool"`
	Command              string   `yaml:"command"`
	HelperTimeoutSeconds *float64 `yaml:"helper_timeout_seconds,omitempty"`
	OverrideExisting     bool     `yaml:"override_existing" coerce:"bool"`
	rest                 rest
}

type secretsCommandFields SecretsCommand

// Decodes the mapping and keeps unknown keys
func (s *SecretsCommand) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*secretsCommandFields)(s), &s.rest)
}

// Writes the mapping with unknown keys after the named ones
func (s SecretsCommand) MarshalYAML() (any, error) {
	return encodeMapping(secretsCommandFields(s), s.rest)
}

// Defaults of the vault section
func defaultVault() VaultConfig {
	return VaultConfig{
		KeyStorage:  VaultKeyFile,
		OnePassword: VaultOnePassword{Enabled: true, ServiceAccountTokenEnv: "OP_SERVICE_ACCOUNT_TOKEN"},
		Bitwarden:   VaultBitwarden{Enabled: true},
	}
}

// Defaults of the secrets section
func defaultSecrets() SecretsConfig {
	return SecretsConfig{
		Bitwarden: SecretsBitwarden{
			AccessTokenEnv: "BWS_ACCESS_TOKEN", CacheTTLSeconds: 300, OverrideExisting: true, AutoInstall: true,
		},
		OnePassword: SecretsOnePassword{
			ServiceAccountTokenEnv: "OP_SERVICE_ACCOUNT_TOKEN", CacheTTLSeconds: 300, OverrideExisting: true,
		},
	}
}

// Documentation of the vault and secrets sections
func secretsDocs() map[string]string {
	return map[string]string{
		"vault":                                         "Browser credential vault: which login sources browser_vault may draw from. The local encrypted vault (`icebath vault add`, Desktop -> Settings -> Credential Vault) is always on. External password managers are unlocked per session with a masked master-password prompt; headless sessions (cron, webhook, API) never prompt and see them as locked.",
		"vault.key_storage":                             "A NEW vault's key: file (0600 vault.key) | keychain (OS; file if none). See `icebath vault key`.",
		"vault.onepassword":                             "Detected managers are login sources unless the user opts out (enabled: false).",
		"vault.onepassword.enabled":                     "`op` CLI: Login items with a website URL become fillable handles.",
		"vault.onepassword.account":                     "Account shorthand for `op --account`; empty = default account.",
		"vault.onepassword.binary_path":                 "Absolute path to op; empty = PATH.",
		"vault.onepassword.service_account_token_env":   "Env var holding a service-account token (headless auth, no unlock prompt). Unset = prompt.",
		"vault.bitwarden.enabled":                       "`bw` CLI (Password Manager, not Secrets Manager); run `bw login` once first.",
		"vault.bitwarden.binary_path":                   "Absolute path to bw; empty = PATH.",
		"secrets":                                       "External secret sources: pull credentials from secret managers at startup instead of storing them in ~/.icebath/.env. An optional `sources` list orders enabled sources (e.g. [onepassword, bitwarden]); default registration order. Mapped sources (explicit VAR -> ref) always beat bulk sources (project dumps); the first claim on a var wins, later ones warn.",
		"secrets.bitwarden.enabled":                     "When false, Bitwarden Secrets Manager is never contacted and bws is never auto-installed.",
		"secrets.bitwarden.access_token_env":            "Env var holding the machine-account token, the one bootstrap secret; lives in ~/.icebath/.env (or the shell), never in config.yaml.",
		"secrets.bitwarden.project_id":                  "UUID of the project to sync from.",
		"secrets.bitwarden.cache_ttl_seconds":           "Seconds to reuse a fresh disk or memory cache entry before contacting Bitwarden again. 0 disables fresh-cache reuse.",
		"secrets.bitwarden.encrypted_cache":             "Last-good fallback for NETWORK and TIMEOUT outages: an AES-GCM cache under ~/.icebath/cache/, reused up to max_stale_seconds. Auth failures never fall back.",
		"secrets.bitwarden.override_existing":           "Secret values overwrite existing env vars, so rotating in Bitwarden takes effect without clearing the matching .env line.",
		"secrets.bitwarden.auto_install":                "Auto-download bws into ~/.icebath/bin/ on first use; false = bws must be on PATH.",
		"secrets.bitwarden.server_url":                  "Passed to bws as BWS_SERVER_URL. Empty = US Cloud (bws default); https://vault.bitwarden.eu for EU; your own URL for self-hosted.",
		"secrets.onepassword.enabled":                   "When false, the op CLI is never invoked.",
		"secrets.onepassword.env":                       "Env-var name -> op://vault/item/field; each resolved with one `op read`.",
		"secrets.onepassword.account":                   "Account shorthand or sign-in address for `op read --account`; empty = default.",
		"secrets.onepassword.service_account_token_env": "Env var holding a service-account token for headless auth (exported to op as OP_SERVICE_ACCOUNT_TOKEN). Unset = interactive or desktop op session.",
		"secrets.onepassword.binary_path":               "Absolute path to op, used verbatim (avoids trusting PATH). Empty = PATH.",
		"secrets.onepassword.cache_ttl_seconds":         "Seconds to cache values in-process and on disk; 0 disables BOTH layers.",
		"secrets.onepassword.override_existing":         "Overwrite existing env vars so rotation takes effect; false lets .env win.",
		"secrets.command":                               "Helper run once at startup via /bin/sh -c that prints a KEY=VALUE blob for many secrets (POSIX only).",
	}
}
