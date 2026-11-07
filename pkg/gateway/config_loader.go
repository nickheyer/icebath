package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Legacy gateway.json base layer under the home, an empty mapping when absent and on a malformed file
func LoadLegacyGatewayJSON(h string) any {
	path := filepath.Join(h, "gateway.json")
	if _, err := os.Stat(path); err != nil {
		return map[string]any{}
	}
	data, err := readLegacyJSON(path)
	if err != nil {
		slog.Warn(fmt.Sprintf("Failed to load %s: %s", path, err))
		return map[string]any{}
	}
	slog.Info(fmt.Sprintf("Loaded legacy %s — consider moving settings to config.yaml", path))
	return data
}

func readLegacyJSON(path string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(raw) {
		return nil, errors.New("'utf-8' codec can't decode the file")
	}
	v, err := pyjson.Loads(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))
	if err != nil {
		return nil, err
	}
	v = plainJSON(v)
	if !pyrepr.Truthy(v) {
		return map[string]any{}, nil
	}
	return v, nil
}

// Ordered JSON objects turned into plain mappings, recursively through lists
func plainJSON(v any) any {
	switch t := v.(type) {
	case *pyjson.Object:
		out := make(map[string]any, t.Len())
		for _, k := range t.Keys {
			val, _ := t.Get(k)
			out[k] = plainJSON(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = plainJSON(e)
		}
		return out
	case *big.Int:
		return t
	}
	return v
}

var bridgedAllowAll struct {
	sync.Mutex
	set bool
}

// GATEWAY_ALLOW_ALL_USERS when the config bridge wrote it, ok false for an operator set or absent value
func BridgedAllowAllUsers() (string, bool) {
	bridgedAllowAll.Lock()
	set := bridgedAllowAll.set
	bridgedAllowAll.Unlock()
	if !set {
		return "", false
	}
	return os.LookupEnv("GATEWAY_ALLOW_ALL_USERS")
}

// Child process env without the bridge owned GATEWAY_ALLOW_ALL_USERS, an operator value stays
func DropBridgedEnv(env map[string]string) map[string]string {
	if _, ok := BridgedAllowAllUsers(); ok {
		delete(env, "GATEWAY_ALLOW_ALL_USERS")
	}
	return env
}

func quickCommandsOK(value any) bool {
	if _, ok := value.(map[string]any); ok {
		return true
	}
	slog.Warn(fmt.Sprintf("Ignoring invalid quick_commands in config.yaml (expected mapping, got %s)", pyrepr.TypeName(value)))
	return false
}

func dmBehaviorChoice(value any) any { return normalizeChoice(value, UnauthorizedDMBehaviors, "pair") }

type toplevelBridgeEntry struct {
	yamlKey, gwKey, mode string
	accept               func(any) bool
	transform            func(any) any
}

func presence(keys ...string) []toplevelBridgeEntry {
	out := make([]toplevelBridgeEntry, len(keys))
	for i, k := range keys {
		out[i] = toplevelBridgeEntry{yamlKey: k, gwKey: k, mode: "presence"}
	}
	return out
}

func isMapping(v any) bool { _, ok := v.(map[string]any); return ok }

func isList(v any) bool { _, ok := v.([]any); return ok }

// Top level settings also accepted nested under gateway, with the fallback mode each one uses
var toplevelBridge = concatBridge(
	[]toplevelBridgeEntry{
		{"quick_commands", "quick_commands", "none", quickCommandsOK, nil},
		{"stt", "stt", "presence", isMapping, nil},
	},
	presence("stt_echo_transcripts", "group_sessions_per_user", "thread_sessions_per_user"),
	[]toplevelBridgeEntry{{"multiplex_profiles", "multiplex_profiles", "gwdata", nil, nil}},
	presence("room_link_url"),
	[]toplevelBridgeEntry{{"profile_routes", "profile_routes", "none", isList, nil}},
	presence("max_concurrent_sessions"),
	[]toplevelBridgeEntry{
		{"systemd_watchdog_seconds", "systemd_watchdog_seconds", "nested", nil, nil},
		{"streaming", "streaming", "dict", nil, nil},
	},
	presence("reset_triggers", "always_log_local", "write_sessions_json", "loop_watchdog",
		"loop_watchdog_probe_interval_s", "loop_watchdog_probe_timeout_s", "loop_watchdog_max_strikes",
		"filter_silence_narration"),
	[]toplevelBridgeEntry{{"unauthorized_dm_behavior", "unauthorized_dm_behavior", "presence", nil, dmBehaviorChoice}},
	presence("unauthorized_dm_decline_message"),
)

func concatBridge(groups ...[]toplevelBridgeEntry) []toplevelBridgeEntry {
	var out []toplevelBridgeEntry
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// Found flag and value of a key under the fallback mode between top level and nested gateway forms
func bridgeLookup(yamlCfg map[string]any, gatewaySection any, gwData map[string]any, key, mode string) (bool, any) {
	nested, isNested := gatewaySection.(map[string]any)
	switch mode {
	case "presence", "gwdata":
		if v, ok := yamlCfg[key]; ok {
			return true, v
		}
		if isNested {
			if v, ok := nested[key]; ok {
				if _, inGw := gwData[key]; mode == "presence" || !inGw {
					return true, v
				}
			}
		}
		return false, nil
	case "nested":
		if isNested {
			if v, ok := nested[key]; ok {
				return true, v
			}
		}
		return false, nil
	}
	value := yamlCfg[key]
	if mode == "none" {
		if value == nil && isNested {
			value = nested[key]
		}
		return value != nil, value
	}
	if !isMapping(value) && isNested {
		value = nested[key]
	}
	return isMapping(value), value
}

// Copies every bridged top level or nested gateway setting into the flat gateway data
func BridgeToplevelKeys(yamlCfg map[string]any, gatewaySection any, gwData map[string]any) {
	for _, e := range toplevelBridge {
		found, value := bridgeLookup(yamlCfg, gatewaySection, gwData, e.yamlKey, e.mode)
		if !found || (e.accept != nil && !e.accept(value)) {
			continue
		}
		if e.transform != nil {
			value = e.transform(value)
		}
		gwData[e.gwKey] = value
	}
}

// True inside a multiplexed secondary profile's scope, where YAML to env bridges must not write
func profileScoped(ctx context.Context) bool {
	return provider.IsMultiplexActive(ctx) && provider.CurrentSecretScope(ctx) != nil
}

// True inside a multiplexed secondary profile's scope, for the platform adapters
func ProfileScoped(ctx context.Context) bool { return profileScoped(ctx) }

// Python membership of a key in a mapping, list element or substring of a string
func pyContains(container any, key string) bool {
	switch c := container.(type) {
	case map[string]any:
		_, ok := c[key]
		return ok
	case []any:
		for _, e := range c {
			if s, ok := e.(string); ok && s == key {
				return true
			}
		}
		return false
	case string:
		return strings.Contains(c, key)
	}
	return false
}

func setDefaultDict(container map[string]any, key string) map[string]any {
	if v, ok := container[key]; ok {
		m, _ := v.(map[string]any)
		return m
	}
	m := map[string]any{}
	container[key] = m
	return m
}

// YAML to env bridges kept in core, allow_all_users, top level require_mention for Telegram and Signal's
func BridgeCoreEnvSettings(ctx context.Context, yamlCfg, platformsData map[string]any) {
	skipEnvBridge := profileScoped(ctx)
	gatewaySection := yamlCfg["gateway"]
	allowAll := yamlCfg["allow_all_users"]
	if nested, ok := gatewaySection.(map[string]any); ok && allowAll == nil {
		allowAll = nested["allow_all_users"]
	}
	bridgedAllowAll.Lock()
	if !skipEnvBridge && (bridgedAllowAll.set || os.Getenv("GATEWAY_ALLOW_ALL_USERS") == "") {
		switch pystr.Lower(pyrepr.Str(allowAll)) {
		case "true", "1", "yes":
			bridgedAllowAll.set = true
		default:
			bridgedAllowAll.set = false
		}
		if bridgedAllowAll.set {
			os.Setenv("GATEWAY_ALLOW_ALL_USERS", "true")
			slog.Warn("config.yaml allow_all_users: true grants every sender on every platform access (bridged to GATEWAY_ALLOW_ALL_USERS; an explicit env var wins).")
		} else {
			os.Unsetenv("GATEWAY_ALLOW_ALL_USERS")
		}
	}
	bridgedAllowAll.Unlock()
	if tl, ok := yamlCfg["require_mention"]; ok && tl != nil {
		telegram := yamlCfg["telegram"]
		if !pyrepr.Truthy(telegram) {
			telegram = map[string]any{}
		}
		if !pyContains(telegram, "require_mention") {
			extra := setDefaultDict(setDefaultDict(platformsData, "telegram"), "extra")
			if _, set := extra["require_mention"]; !set {
				extra["require_mention"] = tl
			}
			if !skipEnvBridge && os.Getenv("TELEGRAM_REQUIRE_MENTION") == "" {
				os.Setenv("TELEGRAM_REQUIRE_MENTION", pystr.Lower(pyrepr.Str(tl)))
			}
		}
	}
	signal, _ := yamlCfg["signal"].(map[string]any)
	if rm, ok := signal["require_mention"]; ok {
		extra := setDefaultDict(setDefaultDict(platformsData, "signal"), "extra")
		if _, set := extra["require_mention"]; !set {
			extra["require_mention"] = rm
		}
		if !skipEnvBridge && os.Getenv("SIGNAL_REQUIRE_MENTION") == "" {
			os.Setenv("SIGNAL_REQUIRE_MENTION", pystr.Lower(pyrepr.Str(rm)))
		}
	}
}
