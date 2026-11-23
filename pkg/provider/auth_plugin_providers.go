package provider

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
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

// Whether a profile signs in through its own device login hooks
func profileHasDeviceLogin(p Profile) bool {
	return p.StartDeviceLogin != nil && p.PollDeviceLogin != nil && p.SaveDeviceLogin != nil
}

// Copy of a profile device login that ended refused or expired, keyed by the status its poll reported
var deviceLoginEndings = map[string]string{"denied": "Authorization was denied.", "expired": "Device code expired before approval"}

// Whole number of a device login reply, text parsed and any other shape refused
func deviceLoginInt(v any) (int, error) {
	if s, ok := v.(string); ok {
		return parsePyInt(s)
	}
	if n, ok := anyInt(v); ok {
		return n, nil
	}
	return 0, fmt.Errorf("a device login value is %s, expected a whole number", errchain.Kind(v))
}

// Required field of a device login reply, an error when the profile left it out
func deviceLoginField(login map[string]any, key string) (any, error) {
	v, ok := login[key]
	if !ok {
		return nil, fmt.Errorf("the device login reply has no %s", key)
	}
	return v, nil
}

// Device code sign in through a start and a poll hook, polled at their pace until it ends or the context stops, the browser opened here when asked; answers the credential the approved sign in produced
func deviceCodeCredential(ctx context.Context, out io.Writer, name string, start func(context.Context) (any, error), poll func(context.Context, any) (any, error), openBrowser bool) (any, error) {
	started, err := start(ctx)
	if err != nil {
		return nil, err
	}
	login := hostedMapOrNil(started)
	if len(login) == 0 {
		return nil, authExit(fmt.Sprintf("Provider %s does not support device-code flow", name))
	}
	fields := map[string]any{}
	for _, key := range []string{"user_code", "verification_url", "expires_in", "interval"} {
		if fields[key], err = deviceLoginField(login, key); err != nil {
			return nil, err
		}
	}
	expiresIn, err := deviceLoginInt(fields["expires_in"])
	if err != nil {
		return nil, err
	}
	interval, err := deviceLoginInt(fields["interval"])
	if err != nil {
		return nil, err
	}
	interval = max(1, interval)
	if err := printDeviceCodeInstructions(out, hostedStr(fields["verification_url"]), hostedStr(fields["user_code"]), openBrowser && !isRemoteSession(), "—", true); err != nil {
		return nil, err
	}
	fmt.Fprintln(out, "Waiting for approval...")
	state := login["state"]
	deadline := time.Now().Add(time.Duration(expiresIn) * time.Second)
	cancelled := func() bool { return ctx.Err() != nil }
	result := map[string]any{}
	for time.Now().Before(deadline) {
		if cancellableSleep(time.Duration(interval)*time.Second, cancelled) != nil {
			return nil, &SignInCancelledError{ExitCode: 130, HasCode: true}
		}
		polled, err := poll(ctx, state)
		if err != nil {
			return nil, err
		}
		result = hostedMap(polled)
		if next, ok := result["state"]; ok {
			state = next
		}
		if hostedStr(result["status"]) != "pending" {
			break
		}
		if truthy(result["interval"]) {
			if interval, err = deviceLoginInt(result["interval"]); err != nil {
				return nil, err
			}
		}
	}
	if cancelled() {
		return nil, &SignInCancelledError{ExitCode: 130, HasCode: true}
	}
	status := hostedStr(result["status"])
	if status == "" || status == "pending" {
		status = "expired"
	}
	if status != "approved" {
		message := hostedStr(result["message"])
		if message == "" {
			message = deviceLoginEndings[status]
		}
		if message == "" {
			message = "Sign-in failed"
		}
		return nil, authExit(message)
	}
	return result["credential"], nil
}

// Device code sign in through the profile's hooks, the approved credential stored by the profile's own save hook
func profileDeviceLogin(ctx context.Context, out io.Writer, name string, profile Profile, openBrowser bool) error {
	credential, err := deviceCodeCredential(ctx, out, name, profile.StartDeviceLogin, profile.PollDeviceLogin, openBrowser)
	if err != nil {
		return err
	}
	if err := profile.SaveDeviceLogin(ctx, credential); err != nil {
		return err
	}
	fmt.Fprintf(out, "Signed in to %s.\n", name)
	return nil
}
