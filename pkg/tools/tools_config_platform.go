package tools

import (
	"context"
	"errors"
	"log/slog"
	"math/big"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Logger the platform toolset warnings go to, swapped by tests capturing records
var toolsConfigLogger = slog.Default

// XAIOAuthTokensReader reads the xAI OAuth tokens from a home's auth store, installed by the provider slice
var XAIOAuthTokensReader func(ctx context.Context, h string) error

var warnedInvalidPlatformToolsets = struct {
	sync.Mutex
	seen map[string]bool
}{seen: map[string]bool{}}

// Marks a platform warned and reports whether it had been warned before
func markPlatformToolsetsWarned(platform string) bool {
	warnedInvalidPlatformToolsets.Lock()
	defer warnedInvalidPlatformToolsets.Unlock()
	before := warnedInvalidPlatformToolsets.seen[platform]
	warnedInvalidPlatformToolsets.seen[platform] = true
	return before
}

func platformToolsetsWarned(platform string) bool {
	warnedInvalidPlatformToolsets.Lock()
	defer warnedInvalidPlatformToolsets.Unlock()
	return warnedInvalidPlatformToolsets.seen[platform]
}

// Clears the once per process warning guard of one platform
func discardPlatformToolsetsWarning(platform string) {
	warnedInvalidPlatformToolsets.Lock()
	delete(warnedInvalidPlatformToolsets.seen, platform)
	warnedInvalidPlatformToolsets.Unlock()
}

var platformEnableEnvVars = [][2]string{
	{"telegram", "TELEGRAM_BOT_TOKEN"}, {"discord", "DISCORD_BOT_TOKEN"}, {"slack", "SLACK_BOT_TOKEN"},
	{"whatsapp", "WHATSAPP_ENABLED"}, {"qqbot", "QQ_APP_ID"},
}

// Platform keys with a configured token, cli always first
func GetEnabledPlatforms(ctx context.Context, h string) ([]string, error) {
	out := []string{"cli"}
	for _, pair := range platformEnableEnvVars {
		v, _, err := config.GetEnvValue(ctx, h, pair[1])
		if err != nil {
			return nil, err
		}
		if v != "" {
			out = append(out, pair[0])
		}
	}
	return out, nil
}

// PlatformToolsetEntry is one platform row of the enabled toolset summary in platform order
type PlatformToolsetEntry struct {
	Platform string
	Enabled  map[string]bool
}

// Enabled toolsets per platform, the configured platforms when platforms is nil
func (r *Registry) PlatformToolsetSummary(ctx context.Context, scope, h string, cfg config.Tree, platforms []string) ([]PlatformToolsetEntry, error) {
	if platforms == nil {
		var err error
		if platforms, err = GetEnabledPlatforms(ctx, h); err != nil {
			return nil, err
		}
	}
	out := make([]PlatformToolsetEntry, 0, len(platforms))
	for _, p := range platforms {
		out = append(out, PlatformToolsetEntry{p, r.PlatformTools(ctx, scope, cfg, p, PlatformToolsOptions{IncludeDefaultMCPServers: true})})
	}
	return out, nil
}

// ParseEnabledFlag reads bool like tool and platform settings, the default for anything unrecognised
func ParseEnabledFlag(value any, def bool) bool {
	switch v := value.(type) {
	case bool:
		return v
	case int:
		return v != 0
	case int64:
		return v != 0
	case int32:
		return v != 0
	case uint64:
		return v != 0
	case *big.Int:
		return v.Sign() != 0
	case string:
		lowered := pystr.Lower(pystr.Strip(v))
		switch lowered {
		case "true", "1", "yes", "on":
			return true
		case "false", "0", "no", "off":
			return false
		}
	}
	return def
}

// Section of the config as a mutable dict, a missing or non dict value replaced by an empty one
func cfgSection(cfg config.Tree, key string) map[string]any {
	switch v := cfg[key].(type) {
	case map[string]any:
		return v
	case config.Tree:
		return map[string]any(v)
	}
	section := map[string]any{}
	cfg[key] = section
	return section
}

// Offline xAI credential check over the auth store tokens, the env file and the scoped secret
func xaiCredentialsPresent(ctx context.Context) bool {
	h := home.Dir(ctx)
	if XAIOAuthTokensReader != nil && XAIOAuthTokensReader(ctx, h) == nil {
		return true
	}
	v, _, err := config.GetEnvValue(ctx, h, "XAI_API_KEY")
	if err != nil {
		panic(err)
	}
	return pystr.Strip(v) != ""
}

// Plugin toolsets on unless default off or known for the platform and absent from the saved list
func enabledPluginToolsets(cfg config.Tree, platform string, names []string, pluginKeys map[string]bool) map[string]bool {
	known := map[string]bool{}
	if kpt, ok := pyDictOf(cfg["known_plugin_toolsets"]); ok {
		switch list := kpt[platform].(type) {
		case []any:
			for _, n := range list {
				known[pyrepr.Str(n)] = true
			}
		case string:
			for _, ch := range list {
				known[string(ch)] = true
			}
		}
	}
	out := map[string]bool{}
	for p := range pluginKeys {
		if containsString(names, p) || (!DefaultOffToolsets[p] && !known[p]) {
			out[p] = true
		}
	}
	return out
}

// EnabledPluginToolsets is the exported plugin toolset resolver for the migration tests of other packages
func EnabledPluginToolsets(cfg config.Tree, platform string, names []string, pluginKeys map[string]bool) map[string]bool {
	return enabledPluginToolsets(cfg, platform, names, pluginKeys)
}

// Whether a non compressor context engine is configured
func contextEngineActive(cfg config.Tree) bool {
	ctxCfg := cfg["context"]
	if !pyTruthy(ctxCfg) {
		return false
	}
	m, ok := pyDictOf(ctxCfg)
	if !ok {
		return false
	}
	engine := m["engine"]
	if !pyTruthy(engine) {
		return false
	}
	name := pystr.Lower(pystr.Strip(pyrepr.Str(engine)))
	return name != "" && name != "compressor"
}

// Dict view of a config value, false for anything that is not a mapping
func pyDictOf(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case config.Tree:
		return map[string]any(m), true
	}
	return nil, false
}

// Saved platform list as names, warning once per platform about a value that is not a list
func coercePlatformToolsetsValue(value any, platform string) ([]string, bool) {
	if value == nil {
		return nil, false
	}
	if names, ok := ParsePlatformToolsetsValue(value); ok {
		return names, true
	}
	if !markPlatformToolsetsWarned(platform) {
		toolsConfigLogger().Warn("platform_toolsets." + platform + " is " + pyrepr.Repr(value) +
			", expected a YAML list of toolset names (e.g. [terminal, file, web]) - falling back to the platform default. Run `icebath tools` to reconfigure.")
	}
	return nil, false
}

// Warns once when every name of an explicit platform list is invalid, leaving the platform toolless
func (r *Registry) warnAllInvalidPlatformToolsets(ctx context.Context, scope, platform string, explicit []string, cfg config.Tree) {
	if platformToolsetsWarned(platform) {
		return
	}
	var named []string
	for _, t := range explicit {
		if t != "" {
			named = append(named, t)
		}
	}
	if len(named) == 0 {
		return
	}
	for _, t := range named {
		if r.ValidateToolset(ctx, scope, t) || IsKnownPlatformBundle(platform, t) {
			return
		}
	}
	isValid := SavedToolsetResolver(ctx, cfg)
	for _, t := range named {
		if isValid(t) {
			return
		}
	}
	markPlatformToolsetsWarned(platform)
	toolsConfigLogger().Warn("platform '" + platform + "' has no valid toolsets configured (unknown name(s): " +
		strings.Join(named, ", ") + ") - tools will be unavailable. Run `icebath tools` to reconfigure. See issue #38798.")
}

// SavedToolsetResolver judges a saved platform_toolsets name the way session tool resolution lets it through
func SavedToolsetResolver(ctx context.Context, cfg config.Tree) func(string) bool {
	known := map[string]bool{"no_mcp": true}
	if servers, ok := pyDictOf(cfg["mcp_servers"]); ok {
		for name := range servers {
			known[name] = true
			known["mcp-"+name] = true
		}
	}
	bundles := sync.OnceValue(func() map[string]bool {
		names, err := config.PlatformPluginManifestDirNames(home.Dir(ctx))
		if err != nil {
			panic(err)
		}
		out := map[string]bool{}
		for _, n := range names {
			out[BundlePrefix+n] = true
		}
		return out
	})
	plugins := sync.OnceValue(func() map[string]bool {
		out := map[string]bool{}
		if PluginToolsetCatalog.KeysNowait != nil {
			for k := range PluginToolsetCatalog.KeysNowait(ctx) {
				out[k] = true
			}
		}
		if PluginToolsetCatalog.PortableNowait != nil {
			for k := range PluginToolsetCatalog.PortableNowait(ctx) {
				out[k] = true
				out["mcp-"+k] = true
			}
		}
		return out
	})
	return func(name string) bool {
		name = CanonicalToolsetName(name)
		return Default.ValidateToolset(ctx, "", name) || known[name] || bundles()[name] || plugins()[name]
	}
}

// Python membership of kanban in a toolsets value, substring for a string and key for a dict
func pyContainsKanban(v any) bool {
	switch c := v.(type) {
	case nil:
		return false
	case []any:
		for _, item := range c {
			if s, ok := item.(string); ok && s == "kanban" {
				return true
			}
		}
		return false
	case []string:
		return containsString(c, "kanban")
	case string:
		return strings.Contains(c, "kanban")
	}
	if m, ok := pyDictOf(v); ok {
		_, has := m["kanban"]
		return has
	}
	if !pyTruthy(v) {
		return false
	}
	panic(errors.New("TypeError: argument of type '" + pyrepr.TypeName(v) + "' is not iterable"))
}

// Whether every env var a provider row declares is set, true for rows without keys
func providerEnvReady(ctx context.Context, h string, provider *pyjson.Object) (bool, error) {
	raw, _ := provider.Get("env_vars")
	vars, _ := raw.([]any)
	for _, e := range vars {
		row, _ := e.(*pyjson.Object)
		if row == nil {
			return false, errors.New("TypeError: env_vars entry is not a dict")
		}
		key, ok := row.Get("key")
		if !ok {
			return false, errors.New("KeyError: 'key'")
		}
		v, _, err := config.GetEnvValue(ctx, h, pyrepr.Str(key))
		if err != nil {
			return false, err
		}
		if v == "" {
			return false, nil
		}
	}
	return true, nil
}

// Collection and send flags of telemetry.shared_metrics, true only for a literal true
func sharedMetricsState(cfg config.Tree) (enabled, send bool) {
	telemetry, ok := pyDictOf(cfg["telemetry"])
	if !ok {
		return false, false
	}
	shared, ok := pyDictOf(telemetry["shared_metrics"])
	if !ok {
		return false, false
	}
	e, _ := shared["enabled"].(bool)
	s, _ := shared["send"].(bool)
	return e, s
}

// Menu row naming the shared metrics state, collect only included for profiles that chose it earlier
func sharedMetricsMenuLabel(cfg config.Tree) string {
	enabled, send := sharedMetricsState(cfg)
	state := "off"
	if enabled {
		state = "collecting locally"
		if send {
			state = "sending to Nous"
		}
	}
	return "Configure shared metrics  (" + state + ")"
}

// Added and removed toolsets limited to the checklist universe so read time names never print as removals
func checklistDiff(ctx context.Context, newEnabled, prev map[string]bool, platform string) (added, removed map[string]bool) {
	universe := checklistToolsetKeys(ctx, platform)
	added, removed = map[string]bool{}, map[string]bool{}
	for k := range newEnabled {
		if !prev[k] && universe[k] {
			added[k] = true
		}
	}
	for k := range prev {
		if !newEnabled[k] && universe[k] {
			removed[k] = true
		}
	}
	return added, removed
}
