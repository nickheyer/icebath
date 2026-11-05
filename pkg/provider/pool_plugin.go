package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ProviderProfileHooks are the optional credential hooks a model provider plugin profile ships
type ProviderProfileHooks struct {
	// Rotates a pooled OAuth row, returning the rotated fields or nothing when it could not
	RefreshCredential func(entry PooledCredential) (map[string]any, error)
	// Rows the profile seeds itself from ambient tokens it validates or exchanges
	SeedPoolCredentials func(suppressedSources map[string]bool, explicitlyConfigured bool) []map[string]any
	// Source variants and hints the profile claims for a removal, nil when it does not claim the source
	RemoveCredentialSource func(source string) ([]string, []string, bool)
	// Owns an icebath auth action, true when core prints nothing more
	AuthHandler func(action string, args []string, provider string) (bool, error)
}

var (
	profileHooksMu sync.RWMutex
	profileHooks   = map[string]*ProviderProfileHooks{}
)

// RegisterProviderProfileHooks installs a plugin profile's credential hooks for a provider
func RegisterProviderProfileHooks(provider string, hooks ProviderProfileHooks) {
	profileHooksMu.Lock()
	defer profileHooksMu.Unlock()
	h := hooks
	profileHooks[strings.ToLower(strings.TrimSpace(provider))] = &h
}

// UnregisterProviderProfileHooks removes a provider's plugin hooks
func UnregisterProviderProfileHooks(provider string) {
	profileHooksMu.Lock()
	defer profileHooksMu.Unlock()
	delete(profileHooks, strings.ToLower(strings.TrimSpace(provider)))
}

func providerProfileHooks(provider string) *ProviderProfileHooks {
	if p := getProviderProfile(context.Background(), strings.ToLower(strings.TrimSpace(provider))); p != nil &&
		(p.RefreshCredential != nil || p.SeedPoolCredentialsHook != nil || p.RemoveCredentialSourceHook != nil || p.AuthHandler != nil) {
		return &ProviderProfileHooks{RefreshCredential: p.RefreshCredential, SeedPoolCredentials: p.SeedPoolCredentialsHook,
			RemoveCredentialSource: p.RemoveCredentialSourceHook, AuthHandler: p.AuthHandler}
	}
	profileHooksMu.RLock()
	defer profileHooksMu.RUnlock()
	return profileHooks[strings.ToLower(strings.TrimSpace(provider))]
}

// The profile's refresh hook, nil when its pooled OAuth rows are not refreshable
func pluginRefreshHook(provider string) func(PooledCredential) (map[string]any, error) {
	hooks := providerProfileHooks(provider)
	if hooks == nil {
		return nil
	}
	return hooks.RefreshCredential
}

// PluginRefreshHookExists reports whether a plugin profile ships refresh_credential for the provider
func PluginRefreshHookExists(provider string) bool { return pluginRefreshHook(provider) != nil }

// IsRefreshableOAuthProvider is the built in refreshable set or a plugin profile shipping refresh_credential
func IsRefreshableOAuthProvider(provider string) bool {
	return RefreshableOAuthProviders[provider] || PluginRefreshHookExists(provider)
}

// PluginAuthHandler is the profile's auth action handler, nil when none
func PluginAuthHandler(provider string) func(action string, args []string, provider string) (bool, error) {
	hooks := providerProfileHooks(provider)
	if hooks == nil {
		return nil
	}
	return hooks.AuthHandler
}

const pluginExpirySkewMS = 120_000

// PluginRowIsExpiring reports an expiry stamped OAuth row due within the skew of expires_at_ms
func PluginRowIsExpiring(entry PooledCredential) bool {
	return entry.ExpiresAtMS != 0 && entry.ExpiresAtMS <= nowFunc().UnixMilli()+pluginExpirySkewMS
}

// ApplyPluginRefreshResult merges a refresh_credential return value into the row, unknown keys into extras
func ApplyPluginRefreshResult(entry PooledCredential, result map[string]any) PooledCredential {
	if len(result) == 0 {
		return entry
	}
	u := entry.Clone()
	for k, v := range result {
		if k == "provider" {
			continue
		}
		if poolFieldNames[k] {
			applyPoolField(&u, k, v)
			continue
		}
		u.Extra[k] = v
	}
	return u
}

// OAuth error codes after which replaying the same refresh token can never succeed
var oauthGrantDeadCodes = map[string]bool{"invalid_grant": true, "invalid_token": true, "refresh_token_reused": true}

// IsTerminalPluginRefreshError is a structured auth error asking for re-login or carrying a grant dead code
func IsTerminalPluginRefreshError(err error) bool {
	var ae *AuthError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.ReloginRequired || oauthGrantDeadCodes[ae.Code]
}

// RecoverFailedPluginRefresh adopts a peer rotation or quarantines a dead grant, handled false means bench
func RecoverFailedPluginRefresh(p *Pool, entry PooledCredential, cause error, held bool) (PooledCredential, bool, bool) {
	synced := p.syncEntryFromPoolStore(entry, held)
	if synced.RefreshToken != entry.RefreshToken && strings.TrimSpace(synced.AccessToken) != "" {
		p.env.log().Debug(fmt.Sprintf("%s refresh failed but the pool store has newer tokens — adopting", p.provider))
		return p.adopt(synced.MarkOK(), held, true), true, true
	}
	if IsTerminalPluginRefreshError(cause) {
		p.env.log().Warn(fmt.Sprintf("%s refresh token for %s is terminally invalid (%v); the credential leaves rotation. Re-run 'icebath auth add %s' to sign in again.", p.provider, entryLabel(entry), cause, p.provider))
		p.markDeadRefreshGrant(entry, cause, held)
		return PooledCredential{}, false, true
	}
	return PooledCredential{}, false, false
}
