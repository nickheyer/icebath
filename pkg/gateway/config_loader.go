package gateway

import (
	"context"
	"fmt"
	"math/big"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Ordered JSON objects turned into plain mappings, recursively through lists
func plainJSON(v any) any {
	switch t := v.(type) {
	case *pyjson.Object:
		out := make(map[string]any, t.Len())
		for _, k := range t.Keys {
			val, _ := t.Get(k)
			out[k] = plainJSON(val)
		}
		config.SetMappingKeys(out, t.Keys)
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

func quickCommandsOK(value any) bool {
	if _, ok := value.(map[string]any); ok {
		return true
	}
	pkgLog().Warn(fmt.Sprintf("Ignoring invalid quick_commands in config.yaml (expected a mapping, got %s)", errchain.Kind(value)))
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

func setDefaultDict(container map[string]any, key string) map[string]any {
	if v, ok := container[key]; ok {
		m, _ := v.(map[string]any)
		return m
	}
	m := map[string]any{}
	container[key] = m
	config.AppendMappingKey(container, key)
	return m
}

// Registers the setting the gateway wide allow all users switch moved to
func init() {
	RegisterEnvSettings(map[string]settings.Entry{"GATEWAY_ALLOW_ALL_USERS": settings.GatewayAllowAllUsers})
}
