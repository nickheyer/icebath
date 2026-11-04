package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Credential source tags for callable keys that mint a bearer on first use
const (
	CredentialSourceEntraID      = "entra-id"
	CredentialSourceVertexOAuth  = "vertex-oauth"
	CredentialSourceMinimaxOAuth = "minimax-oauth"
)

// Credential source to display label, both display surfaces resolve labels here
var credentialSourceLabels = map[string]string{
	CredentialSourceEntraID: "Microsoft Entra ID", CredentialSourceVertexOAuth: "Google Vertex OAuth",
	CredentialSourceKeyCmd: "key_cmd token", CredentialSourceMinimaxOAuth: "MiniMax OAuth",
}

// Neutral label for an untagged token source, never another vendor's name
const credentialSourceUnknownLabel = "token provider"

// TaggedTokenSource is a token minter that names its credential source without being called
type TaggedTokenSource struct {
	Mint   func(ctx context.Context) (string, error)
	Source string
}

// Token mints through the wrapped minter
func (t *TaggedTokenSource) Token(ctx context.Context) (string, error) { return t.Mint(ctx) }

// TagCredentialSource records the source on a minter and returns a callable token source
func TagCredentialSource(mint func(ctx context.Context) (string, error), source string) *TaggedTokenSource {
	return &TaggedTokenSource{Mint: mint, Source: source}
}

// CredentialSourceOf is the source tag of a callable key, empty for strings and untagged minters
func CredentialSourceOf(key any) string {
	switch k := key.(type) {
	case *TaggedTokenSource:
		if k != nil {
			return k.Source
		}
	case *CommandTokenSource:
		if k != nil {
			return CredentialSourceKeyCmd
		}
	}
	return ""
}

// CredentialLabel is the label for a callable key's source, empty for a non callable
func CredentialLabel(key any) string {
	switch k := key.(type) {
	case string, nil:
		return ""
	case *TaggedTokenSource, *CommandTokenSource, func(context.Context) (string, error):
		_ = k
	default:
		return ""
	}
	if label, ok := credentialSourceLabels[CredentialSourceOf(key)]; ok {
		return label
	}
	return credentialSourceUnknownLabel
}

// ---- credential_sources.py ----

// ExternalLoginsNotAdoptedNotice is printed once per process when a borrowed CLI login is skipped
const ExternalLoginsNotAdoptedNotice = "External CLI logins (Codex CLI, Claude Code) are not adopted: auth.adopt_external_logins is false. " +
	"Icebath uses only its own logins; run `icebath auth add <provider>` to add one."

var (
	externalLoginsNoticeMu     sync.Mutex
	externalLoginsNoticeLogged bool
)

// AdoptExternalLoginsEnabled is auth.adopt_external_logins, default true, logged once when off
func AdoptExternalLoginsEnabled(env PoolEnv) bool {
	cfg := env.config()
	authCfg, ok := cfg["auth"].(map[string]any)
	enabled := true
	if ok {
		if v, has := authCfg["adopt_external_logins"]; has {
			enabled = truthyAny(v)
		}
	}
	if !enabled {
		externalLoginsNoticeMu.Lock()
		if !externalLoginsNoticeLogged {
			externalLoginsNoticeLogged = true
			env.log().Info(ExternalLoginsNotAdoptedNotice)
		}
		externalLoginsNoticeMu.Unlock()
	}
	return enabled
}

// RemovalResult is the outcome of removing a credential source
type RemovalResult struct {
	Cleaned  []string
	Hints    []string
	Suppress bool
}

// RemovalStep says how to remove one credential source
type RemovalStep struct {
	Provider    string
	SourceID    string
	Remove      func(env PoolEnv, provider string, removed PooledCredential) RemovalResult
	Match       func(source string) bool
	Description string
}

// Matches reports whether the step covers the provider and source
func (s RemovalStep) Matches(provider, source string) bool {
	if s.Provider != "*" && s.Provider != provider {
		return false
	}
	if s.Match != nil {
		return s.Match(source)
	}
	return source == s.SourceID
}

// FindRemovalStep is the profile's own step when it claims the source, else the first registry match
func FindRemovalStep(provider, source string) (RemovalStep, bool) {
	if step, ok := profileRemovalStep(provider, source); ok {
		return step, true
	}
	for _, step := range removalRegistry {
		if step.Matches(provider, source) {
			return step, true
		}
	}
	return RemovalStep{}, false
}

// A profile seeding one token under several sources suppresses every variant
func profileRemovalStep(provider, source string) (RemovalStep, bool) {
	hooks := providerProfileHooks(provider)
	if hooks == nil || hooks.RemoveCredentialSource == nil {
		return RemovalStep{}, false
	}
	sources, hints, claimed := hooks.RemoveCredentialSource(source)
	if !claimed {
		return RemovalStep{}, false
	}
	return RemovalStep{Provider: provider, SourceID: source, Remove: func(env PoolEnv, provider string, removed PooledCredential) RemovalResult {
		for _, variant := range sources {
			_ = env.Store.SuppressCredentialSource(provider, variant)
		}
		return RemovalResult{Hints: append([]string(nil), hints...), Suppress: true}
	}}, true
}

// Clears an env seeded credential from the profile .env and hints when the shell still exports it
func removeEnvSource(env PoolEnv, provider string, removed PooledCredential) RemovalResult {
	result := RemovalResult{Suppress: true}
	envVar := strings.TrimPrefix(removed.Source, "env:")
	if envVar == "" {
		return result
	}
	inProcess := os.Getenv(envVar) != ""
	inDotenv := false
	envPath := filepath.Join(env.Store.Home(), ".env")
	if raw, err := os.ReadFile(envPath); err == nil {
		for _, line := range strings.Split(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), envVar+"=") {
				inDotenv = true
				break
			}
		}
	}
	if removedFromFile, _ := RemoveEnvSetting(env.Store.Home(), envVar); removedFromFile {
		result.Cleaned = append(result.Cleaned, fmt.Sprintf("Cleared %s from .env", envVar))
	}
	if inProcess && !inDotenv {
		result.Hints = append(result.Hints,
			fmt.Sprintf("Note: %s is still set in your shell environment (not in ~/.icebath/.env).", envVar),
			"  Unset it there (shell profile, systemd EnvironmentFile, launchd plist, etc.) or it will keep being visible to Icebath.",
			fmt.Sprintf("  The pool entry is now suppressed — Icebath will ignore %s until you run `icebath auth add %s`.", envVar, provider))
	} else {
		result.Hints = append(result.Hints, fmt.Sprintf("Suppressed env:%s — it will not be re-seeded even if the variable is re-exported later.", envVar))
	}
	return result
}

// Deletes the icebath owned PKCE file outright
func removeHermesPKCE(env PoolEnv, provider string, removed PooledCredential) RemovalResult {
	result := RemovalResult{Suppress: true}
	oauthFile := HermesOAuthFile(env)
	if _, err := os.Stat(oauthFile); err == nil {
		if err := os.Remove(oauthFile); err != nil {
			result.Hints = append(result.Hints, fmt.Sprintf("Could not delete %s: %v", oauthFile, err))
		} else {
			result.Cleaned = append(result.Cleaned, "Cleared Icebath Anthropic OAuth credentials")
		}
	}
	return result
}

// Clears the auth.json providers block of an OAuth provider
func removeAuthStoreOAuth(env PoolEnv, provider string, removed PooledCredential) RemovalResult {
	result := RemovalResult{Suppress: true}
	err := env.Store.Update(func(data AuthData) (bool, error) {
		providers, ok := data["providers"].(map[string]any)
		if !ok {
			return false, nil
		}
		if _, has := providers[provider]; !has {
			return false, nil
		}
		delete(providers, provider)
		result.Cleaned = append(result.Cleaned, fmt.Sprintf("Cleared %s OAuth tokens from auth store", provider))
		return true, nil
	})
	if err != nil {
		result.Hints = append(result.Hints, fmt.Sprintf("Could not update %s: %v", env.Store.Path(), err))
	}
	return result
}

func removeXAIOAuthDeviceCode(env PoolEnv, provider string, removed PooledCredential) RemovalResult {
	result := removeAuthStoreOAuth(env, provider, removed)
	result.Hints = append(result.Hints, "Run `icebath model` → xAI Grok OAuth (SuperGrok / Premium+) to re-authenticate if needed.")
	return result
}

// Codex tokens also live in the Codex CLI's own file which is kept, the canonical source is suppressed
func removeCodexDeviceCode(env PoolEnv, provider string, removed PooledCredential) RemovalResult {
	result := removeAuthStoreOAuth(env, provider, removed)
	_ = env.Store.SuppressCredentialSource(provider, "device_code")
	result.Hints = append(result.Hints,
		"Suppressed openai-codex device_code source — it will not be re-seeded.",
		"Note: Codex CLI credentials still live in ~/.codex/auth.json",
		"Run `icebath auth add openai-codex` to re-enable if needed.")
	return result
}

// Removal that never deletes a file another tool owns, suppress and explain only
func suppressOnly(hints ...string) func(PoolEnv, string, PooledCredential) RemovalResult {
	return func(env PoolEnv, provider string, removed PooledCredential) RemovalResult {
		out := RemovalResult{Suppress: true}
		for _, h := range hints {
			out.Hints = append(out.Hints, strings.ReplaceAll(h, "{source}", removed.Source))
		}
		return out
	}
}

// Order matters, the first match after a profile claim wins
var removalRegistry = []RemovalStep{
	{Provider: "*", SourceID: "env:", Match: func(src string) bool { return strings.HasPrefix(src, "env:") }, Remove: removeEnvSource,
		Description: "Any env-seeded credential (XAI_API_KEY, DEEPSEEK_API_KEY, etc.)"},
	{Provider: "anthropic", SourceID: "claude_code", Remove: suppressOnly(
		"Suppressed claude_code credential — it will not be re-seeded.",
		"Note: Claude Code credentials still live in ~/.claude/.credentials.json",
		"Run `icebath auth add anthropic` to re-enable if needed."), Description: "~/.claude/.credentials.json"},
	{Provider: "anthropic", SourceID: "hermes_pkce", Remove: removeHermesPKCE, Description: "~/.icebath/.anthropic_oauth.json"},
	{Provider: "nous", SourceID: "device_code", Remove: removeAuthStoreOAuth, Description: "auth.json providers.nous"},
	{Provider: "openai-codex", SourceID: "device_code", Match: func(src string) bool { return src == "device_code" || strings.HasSuffix(src, ":device_code") },
		Remove: removeCodexDeviceCode, Description: "auth.json providers.openai-codex + ~/.codex/auth.json"},
	{Provider: "xai-oauth", SourceID: "device_code", Remove: removeXAIOAuthDeviceCode, Description: "auth.json providers.xai-oauth"},
	{Provider: "qwen-oauth", SourceID: "qwen-cli", Remove: suppressOnly(
		"Suppressed qwen-cli credential — it will not be re-seeded.",
		"Note: Qwen CLI credentials still live in ~/.qwen/oauth_creds.json",
		"Run `icebath auth add qwen-oauth` to re-enable if needed."), Description: "~/.qwen/oauth_creds.json"},
	{Provider: "minimax-oauth", SourceID: "oauth", Remove: removeAuthStoreOAuth, Description: "auth.json providers.minimax-oauth"},
	{Provider: "*", SourceID: "config:", Match: func(src string) bool { return strings.HasPrefix(src, "config:") || src == "model_config" },
		Remove: suppressOnly(
			"Suppressed {source} — it will not be re-seeded.",
			"Note: The underlying value in config.yaml is unchanged.  Edit it directly if you want to remove the credential from disk."),
		Description: "Custom provider config.yaml api_key field"},
}

// RemovalSteps lists the registered removal steps in match order
func RemovalSteps() []RemovalStep { return append([]RemovalStep(nil), removalRegistry...) }
