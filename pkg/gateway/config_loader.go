package gateway

import (
	"context"
	"fmt"
	"math/big"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/provider"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Ordered JSON objects turned into plain mappings, recursively through lists
func plainJSON(v any) any {
	switch t := v.(type) {
	case *jsonutil.Object:
		out := make(map[string]any, t.Len())
		for _, k := range t.Keys() {
			val, _ := t.Get(k)
			out[k] = plainJSON(val)
		}
		config.SetMappingKeys(out, t.Keys())
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

// Where a bridged setting lives in config.yaml
type bridgePlace int

const (
	// At the root of config.yaml
	atRoot bridgePlace = iota
	// Under the gateway section
	inGateway
	// Under the gateway section, unless gateway.json already holds it
	inGatewayUnlessData
)

// One config.yaml setting copied into the flat gateway data, with its check and normalization
type toplevelBridgeEntry struct {
	key       string
	place     bridgePlace
	accept    func(any) bool
	transform func(any) any
}

// Entries of the gateway section copied as they are
func inGatewayKeys(keys ...string) []toplevelBridgeEntry {
	out := make([]toplevelBridgeEntry, len(keys))
	for i, k := range keys {
		out[i] = toplevelBridgeEntry{key: k, place: inGateway}
	}
	return out
}

func isMapping(v any) bool { _, ok := v.(map[string]any); return ok }

func isList(v any) bool { _, ok := v.([]any); return ok }

// Settings the gateway data takes from config.yaml, each at the one place the settings registry names
var toplevelBridge = append([]toplevelBridgeEntry{
	{"quick_commands", atRoot, func(v any) bool { return v != nil && quickCommandsOK(v) }, nil},
	{"stt", atRoot, isMapping, nil},
	{"streaming", atRoot, isMapping, nil},
	{"max_concurrent_sessions", atRoot, nil, nil},
	{"multiplex_profiles", inGatewayUnlessData, nil, nil},
	{"profile_routes", inGateway, isList, nil},
	{"unauthorized_dm_behavior", inGateway, nil, dmBehaviorChoice},
}, inGatewayKeys("group_sessions_per_user", "thread_sessions_per_user", "room_link_url", "systemd_watchdog_seconds",
	"always_log_local", "write_sessions_json", "loop_watchdog", "loop_watchdog_probe_interval_s", "loop_watchdog_probe_timeout_s",
	"loop_watchdog_max_strikes", "filter_silence_narration", "unauthorized_dm_decline_message")...)

// Found flag and value of an entry at its place in config.yaml
func bridgeLookup(yamlCfg map[string]any, gatewaySection any, gwData map[string]any, e toplevelBridgeEntry) (bool, any) {
	source := yamlCfg
	if e.place != atRoot {
		nested, ok := gatewaySection.(map[string]any)
		if !ok {
			return false, nil
		}
		if _, inData := gwData[e.key]; e.place == inGatewayUnlessData && inData {
			return false, nil
		}
		source = nested
	}
	v, ok := source[e.key]
	return ok, v
}

// Copies every bridged config.yaml setting into the flat gateway data
func BridgeToplevelKeys(yamlCfg map[string]any, gatewaySection any, gwData map[string]any) {
	for _, e := range toplevelBridge {
		found, value := bridgeLookup(yamlCfg, gatewaySection, gwData, e)
		if !found || (e.accept != nil && !e.accept(value)) {
			continue
		}
		if e.transform != nil {
			value = e.transform(value)
		}
		gwData[e.key] = value
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
