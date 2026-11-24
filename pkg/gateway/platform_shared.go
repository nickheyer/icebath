package gateway

import (
	"context"
	"os"
	"strconv"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Scoped secret reader the shared platform helpers go through, replaced by tests at this seam
var sharedGetScopedSecret = GetScopedSecret

// Context whose settings reads resolve in the profile a platform task serves, the secret scope's home when no home is bound
func SettingsContext(ctx context.Context) context.Context {
	if home.Override(ctx) == "" {
		if h := provider.CurrentSecretScopeHome(ctx); h != "" {
			return home.WithOverride(ctx, h)
		}
	}
	return ctx
}

// Setting an env name is the override of, which platform declarations, access checks and setup flows use as its identifier; nil when the name is no setting
func envSetting(name string) settings.Entry { return settings.ByEnv(name) }

// Text an env name reads as when it is the override of a setting, for the profile ctx serves: the setting's text, said the other way round for a switch its env name turns off
func envText(ctx context.Context, e settings.Entry) string {
	text := SettingText(ctx, e)
	if on, isBool := e.(*settings.Bool); isBool && on.EnvInvert && text != "" {
		return strconv.FormatBool(!on.Get(SettingsContext(ctx)))
	}
	return text
}

// Items a list setting holds for the profile ctx serves, each trimmed and blank ones dropped; nil for a setting that is no list
func SettingList(ctx context.Context, e settings.Entry) []string {
	list, ok := e.(*settings.List)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range list.Strings(SettingsContext(ctx)) {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// Set of the items a list setting holds for the profile ctx serves
func SettingSet(ctx context.Context, e settings.Entry) map[string]bool {
	out := map[string]bool{}
	for _, item := range SettingList(ctx, e) {
		out[item] = true
	}
	return out
}

// Value of an env name that is the override of a setting, the setting's text for the profile ctx serves, folded false for a name that is no setting
func FoldedEnvValue(ctx context.Context, name string) (value string, folded bool) {
	e := envSetting(name)
	if e == nil {
		return "", false
	}
	return envText(ctx, e), true
}

// Text of a setting the profile ctx serves set: a list comma joined, a switch true or false, a length of time in seconds, empty while only the default applies
func SettingText(ctx context.Context, e settings.Entry) string {
	sctx := SettingsContext(ctx)
	if v, err := settings.Resolve(sctx, e.Info().Key); err != nil || v.Source == settings.SourceDefault {
		return ""
	}
	switch t := e.(type) {
	case *settings.Bool:
		return strconv.FormatBool(t.Get(sctx))
	case *settings.Enum:
		return t.Get(sctx)
	case *settings.Int:
		return strconv.Itoa(t.Get(sctx))
	case *settings.Float:
		return strconv.FormatFloat(t.Get(sctx), 'f', -1, 64)
	case *settings.Duration:
		return strconv.FormatFloat(t.Get(sctx).Seconds(), 'f', -1, 64)
	case *settings.String:
		return t.Get(sctx)
	case *settings.Path:
		return t.Get(sctx)
	case *settings.Secret:
		return t.Get(sctx)
	case *settings.List:
		return strings.Join(SettingList(ctx, t), ",")
	}
	return ""
}

// Allow or deny gate value of the profile ctx serves: the setting a folded name moved to, else the env isolated per profile
func PlatformGateEnv(ctx context.Context, name, def string) string {
	if name == "" {
		return def
	}
	if e := envSetting(name); e != nil {
		if v := strings.TrimSpace(envText(ctx, e)); v != "" {
			return v
		}
		return def
	}
	if scope := provider.CurrentSecretScope(ctx); scope != nil && provider.IsMultiplexActive(ctx) {
		val, ok := scope[name]
		if !ok {
			return def
		}
		return strings.TrimSpace(val)
	}
	val := os.Getenv(name)
	if val == "" {
		val = def
	}
	return strings.TrimSpace(val)
}

// JSON list literal of an allowlist decoded, anything else returned unchanged
func DecodeJSONListLiteral(raw any) any {
	s, ok := raw.(string)
	if !ok || !strings.HasPrefix(strings.TrimLeftFunc(s, unicode.IsSpace), "[") {
		return raw
	}
	loaded, err := pyjson.Loads(s)
	if err != nil {
		return raw
	}
	if list, ok := loaded.([]any); ok {
		return list
	}
	return raw
}

// Per profile setting from the scoped env or the setting a folded name moved to, then the profile's YAML extra, then the default
func ExtraOrSecret(ctx context.Context, extra map[string]any, key, env string, def any, blankIsUnset bool) (any, error) {
	if env != "" {
		val, err := sharedGetScopedSecret(ctx, env, "", false)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(val) != "" {
			return val, nil
		}
	}
	value := extra[key]
	if s, isStr := value.(string); value == nil || (blankIsUnset && isStr && strings.TrimSpace(s) == "") {
		return def, nil
	}
	return value, nil
}

// Failure envelope of a standalone sender, wired by the message tools package
var SendMessageSendersError func(message string) map[string]any

// Standalone sender failure envelope with vendor exception text redacted
func SendError(message any) map[string]any {
	if SendMessageSendersError == nil {
		return map[string]any{"error": "the sender failure envelope is unset, pkg/gateway/messagetools binds it in every build that links the platform senders"}
	}
	return SendMessageSendersError(pyrepr.Str(message))
}

// Active profile's adapter of the in process gateway for a platform, wired by the message tools package
var SendMessageSendersLiveAdapter func(ctx context.Context, platform Platform) PlatformAdapter

// One env var seeded into an extra key, Conv nil keeping the stripped string and a ValueError skipping it
type EnvSeedRow struct {
	Env, Key string
	Conv     func(raw string) (any, error)
}

// Extra seeded from the profile scoped env, with the home channel from homeEnv and its name var
func SeedExtraFromEnv(ctx context.Context, spec []EnvSeedRow, homeEnv, homeDefault string) (map[string]any, error) {
	seed := map[string]any{}
	for _, row := range spec {
		val, err := sharedGetScopedSecret(ctx, row.Env, "", false)
		if err != nil {
			return nil, err
		}
		raw := strings.TrimSpace(val)
		if raw == "" {
			continue
		}
		if row.Conv == nil {
			seed[row.Key] = raw
			continue
		}
		converted, err := row.Conv(raw)
		if pe, ok := err.(*PyError); ok && pe.Kind == "ValueError" {
			continue
		} else if err != nil {
			return nil, err
		}
		seed[row.Key] = converted
	}
	if homeEnv != "" {
		val, err := sharedGetScopedSecret(ctx, homeEnv, "", false)
		if err != nil {
			return nil, err
		}
		chat := strings.TrimSpace(val)
		if chat == "" {
			chat = homeDefault
		}
		if chat != "" {
			name, err := sharedGetScopedSecret(ctx, homeEnv+"_NAME", "Home", false)
			if err != nil {
				return nil, err
			}
			seed["home_channel"] = map[string]any{"chat_id": chat, "name": name}
		}
	}
	return seed, nil
}

// Connected check for env only platforms, every named var non blank through the gateway facade env reader at call time
func EnvIsConnected(names ...string) func(ctx context.Context, cfg *PlatformConfig) (bool, error) {
	return func(ctx context.Context, cfg *PlatformConfig) (bool, error) {
		for _, name := range names {
			val, err := gatewayFacade.getEnvValue(ctx, name)
			if err != nil {
				return false, err
			}
			if strings.TrimSpace(val) == "" {
				return false, nil
			}
		}
		return true, nil
	}
}

// Config connected check true for a token, an api key or every named extra key
func ConfigConnectedExtra(keys ...string) func(cfg *PlatformConfig) bool {
	return func(cfg *PlatformConfig) bool {
		if pyrepr.Truthy(cfg.Token) || pyrepr.Truthy(cfg.APIKey) {
			return true
		}
		for _, k := range keys {
			if !pyrepr.Truthy(cfg.Extra[k]) {
				return false
			}
		}
		return true
	}
}

// Python int of the value or the default when it does not convert or fit
func CoercePort(value any, def int) int {
	n, err := pyint.Of(value)
	if err != nil || !n.IsInt64() {
		return def
	}
	return int(n.Int64())
}

// Host and port the api_server platform binds, platform extra winning over API_SERVER_HOST and API_SERVER_PORT of the profile ctx serves
func APIServerListenAddress(ctx context.Context, extra map[string]any) (any, int) {
	sctx := SettingsContext(ctx)
	lookup := config.ProfileEnvLookup(sctx, home.Dir(sctx))
	host, ok := extra["host"]
	if !ok {
		host = "127.0.0.1"
		if v, set := lookup("API_SERVER_HOST"); set {
			host = v
		}
	}
	rawPort, ok := extra["port"]
	if !ok || rawPort == nil {
		rawPort = "8642"
		if v, set := lookup("API_SERVER_PORT"); set {
			rawPort = v
		}
	}
	return host, CoercePort(rawPort, 8642)
}
