package provider

import (
	"fmt"
	"reflect"
	"strings"
	"time"
)

// Auth actions a provider plugin's auth handler can own
var PluginAuthActions = []string{"add", "status", "logout", "refresh"}

// Auth type a plugin profile's registry row carries, the registry override winning over the declared type
func registryAuthType(p Profile) string {
	if t := profileRegistryAuthType(p); t != "" {
		return t
	}
	return p.AuthType
}

// Registered provider profile for a provider id or alias
func pluginProfile(provider string) (Profile, bool) { return ProfileFor(provider) }

// PluginAuthError is a provider auth handler failure surfaced as a readable exit naming provider and action
type PluginAuthError struct{ Message string }

func (e *PluginAuthError) Error() string { return e.Message }

// Offers an auth action to the provider's plugin OAuth client or auth handler, true when the plugin owned it
func dispatchPluginAuth(action string, args []string, provider string) (bool, error) {
	if owned, handled, err := pluginOAuthHandleAuthAction(action, args, provider); err != nil || handled {
		return owned, err
	}
	handler := PluginAuthHandler(provider)
	if handler == nil {
		return false, nil
	}
	owned, err := handler(action, args, provider)
	if err != nil {
		if exit, ok := err.(*OAuthLoginExitError); ok {
			return false, exit
		}
		if exit, ok := err.(*PluginAuthError); ok {
			return false, exit
		}
		return false, &PluginAuthError{Message: fmt.Sprintf("%s auth handler failed for `%s`: %s: %s", provider, action, goErrorTypeName(err), err)}
	}
	return owned, nil
}

// Type name of an error value without its package and pointer marks
func goErrorTypeName(err error) string {
	t := reflect.TypeOf(err)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Name()
}

// Loud failure for a mirrored non api key plugin that ships no auth handler, nil otherwise
func pluginMissingAuthHandlerError(provider, action string) error {
	if !pluginMirroredProvider(provider) {
		return nil
	}
	profile, ok := pluginProfile(provider)
	if !ok || registryAuthType(profile) == AuthTypeAPIKey || PluginAuthHandler(provider) != nil {
		return nil
	}
	return &PluginAuthError{Message: fmt.Sprintf("Provider '%s' declares auth_type '%s' but its plugin ships no "+
		"auth_handler, so `icebath auth %s %s` cannot be handled. Add "+
		"`auth_handler=` to its ProviderProfile (see the model-provider plugin guide).", provider, profile.AuthType, action, provider)}
}

// True when a pooled OAuth row's millisecond or ISO expiry is in the past
func poolEntryExpired(entry PooledCredential) bool {
	if entry.ExpiresAtMS != 0 {
		return entry.ExpiresAtMS <= time.Now().UnixMilli()
	}
	if entry.ExpiresAt != "" {
		epoch, ok := ParseISOTimestamp(entry.ExpiresAt)
		return ok && epoch <= nowEpoch()
	}
	return false
}

// Status of an OAuth shaped plugin provider read from the credential pool its auth handler fills
func getPluginOAuthAuthStatus(env PoolEnv, provider string) map[string]any {
	if !pluginMirroredProvider(provider) {
		return map[string]any{"logged_in": false}
	}
	var entries, live, refreshable []PooledCredential
	if pool := LoadPool(env, provider); pool != nil {
		for _, e := range pool.Entries() {
			token := e.AccessToken
			if token == "" {
				token = e.AgentKey
			}
			if strings.TrimSpace(token) != "" {
				entries = append(entries, e)
			}
		}
	}
	for _, e := range entries {
		if !poolEntryExpired(e) {
			live = append(live, e)
		}
	}
	if len(live) == 0 && pluginRefreshHook(env.ctx(), provider) != nil {
		for _, e := range entries {
			if e.RefreshToken != "" {
				refreshable = append(refreshable, e)
			}
		}
	}
	baseURL := ""
	for _, e := range append(append([]PooledCredential{}, live...), refreshable...) {
		if e.BaseURL != "" {
			baseURL = e.BaseURL
			break
		}
	}
	hint := ""
	if len(live) == 0 {
		hint = fmt.Sprintf("Run `icebath auth add %s` to sign in.", provider)
	}
	return map[string]any{
		"configured": true, "provider": provider, "logged_in": len(live) > 0, "needs_refresh": len(refreshable) > 0,
		"accounts": len(entries), "base_url": baseURL, "hint": hint,
	}
}
