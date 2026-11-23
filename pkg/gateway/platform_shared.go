package gateway

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
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

// Settings that took over env names, keyed by the name
var envSettings = struct {
	sync.RWMutex
	byName map[string]settings.Entry
}{byName: map[string]settings.Entry{}}

// Records the settings that took over env names, which platform declarations, access checks and setup flows still use as identifiers
func RegisterEnvSettings(byName map[string]settings.Entry) {
	envSettings.Lock()
	defer envSettings.Unlock()
	for name, e := range byName {
		envSettings.byName[name] = e
	}
}

// Setting that took over an env name, nil when the name is still read from the env
func envSetting(name string) settings.Entry {
	envSettings.RLock()
	defer envSettings.RUnlock()
	return envSettings.byName[name]
}

// Items a list setting holds for the profile ctx serves, a comma separated or JSON list text split the same way
func SettingList(ctx context.Context, e settings.Entry) []string {
	v, err := settings.Resolve(SettingsContext(ctx), e.Info().Key)
	if err != nil {
		return nil
	}
	var out []string
	for _, item := range config.AsStrings(DecodeJSONListLiteral(v.Value)) {
		if item = pystr.Strip(item); item != "" {
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

// Value of an env name folded into a setting, the setting's text for the profile ctx serves, folded false for a name still read from the env
func FoldedEnvValue(ctx context.Context, name string) (value string, folded bool) {
	e := envSetting(name)
	if e == nil {
		return "", false
	}
	return SettingText(ctx, e), true
}

// Text of a setting the profile ctx serves set: a list comma joined, a switch true or false, empty while only the default applies
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
	}
	return strings.Join(SettingList(ctx, e), ",")
}

// Allow or deny gate value of the profile ctx serves: the setting a folded name moved to, else the env isolated per profile
func PlatformGateEnv(ctx context.Context, name, def string) string {
	if name == "" {
		return def
	}
	if e := envSetting(name); e != nil {
		if v := pystr.Strip(SettingText(ctx, e)); v != "" {
			return v
		}
		return def
	}
	if scope := provider.CurrentSecretScope(ctx); scope != nil && provider.IsMultiplexActive(ctx) {
		val, ok := scope[name]
		if !ok {
			return def
		}
		return pystr.Strip(val)
	}
	val := os.Getenv(name)
	if val == "" {
		val = def
	}
	return pystr.Strip(val)
}

// JSON list literal of an allowlist decoded, anything else returned unchanged
func DecodeJSONListLiteral(raw any) any {
	s, ok := raw.(string)
	if !ok || !strings.HasPrefix(pystr.Lstrip(s), "[") {
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
		if pystr.Strip(val) != "" {
			return val, nil
		}
	}
	value := extra[key]
	if s, isStr := value.(string); value == nil || (blankIsUnset && isStr && pystr.Strip(s) == "") {
		return def, nil
	}
	return value, nil
}

// Env setter for YAML config hooks that never overrides an explicit env and never writes under a secondary scope
func YAMLEnvSetter(ctx context.Context) func(name string, value any) {
	skip := profileScoped(ctx)
	return func(name string, value any) {
		if value == nil || skip || os.Getenv(name) != "" {
			return
		}
		if list, ok := value.([]any); ok {
			parts := make([]string, len(list))
			for i, v := range list {
				parts[i] = pyrepr.Str(v)
			}
			os.Setenv(name, strings.Join(parts, ","))
			return
		}
		os.Setenv(name, pyrepr.Str(value))
	}
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

// One YAML key bridged to an env var by its kind, lower, str, csv or json
type YAMLBridgeRow struct {
	Key, Env, Kind string
}

// Whether the kind applies to the key of the config and the env value it encodes to
func yamlKind(kind string, cfg map[string]any, key string) (applies bool, encode func(any) (any, error)) {
	_, present := cfg[key]
	switch kind {
	case "lower":
		return present, func(v any) (any, error) { return pystr.Lower(pyrepr.Str(v)), nil }
	case "str":
		v := cfg[key]
		return v != nil && !pyrepr.Equal(v, ""), func(v any) (any, error) { return pyrepr.Str(v), nil }
	case "csv":
		return cfg[key] != nil, func(v any) (any, error) { return v, nil }
	case "json":
		return present, func(v any) (any, error) { return pyjson.Dumps(v) }
	}
	panic(&PyError{"KeyError", pyrepr.Repr(kind)})
}

// Seeds each matching YAML value into the returned extra and bridges it to env, nil when nothing matched
func ApplyYAMLBridge(ctx context.Context, cfg map[string]any, spec []YAMLBridgeRow) (map[string]any, error) {
	setEnv := YAMLEnvSetter(ctx)
	seeded := map[string]any{}
	for _, row := range spec {
		applies, encode := yamlKind(row.Kind, cfg, row.Key)
		if !applies {
			continue
		}
		seeded[row.Key] = cfg[row.Key]
		encoded, err := encode(cfg[row.Key])
		if err != nil {
			return nil, err
		}
		setEnv(row.Env, encoded)
	}
	if len(seeded) == 0 {
		return nil, nil
	}
	return seeded, nil
}

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
		raw := pystr.Strip(val)
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
		chat := pystr.Strip(val)
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
			if pystr.Strip(val) == "" {
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

// Host and port the api_server platform binds, platform extra winning over the API_SERVER env fallbacks
func APIServerListenAddress(extra map[string]any) (any, int) {
	host, ok := extra["host"]
	if !ok {
		host = "127.0.0.1"
		if v, set := os.LookupEnv("API_SERVER_HOST"); set {
			host = v
		}
	}
	rawPort, ok := extra["port"]
	if !ok || rawPort == nil {
		rawPort = "8642"
		if v, set := os.LookupEnv("API_SERVER_PORT"); set {
			rawPort = v
		}
	}
	return host, CoercePort(rawPort, 8642)
}
