package config

import (
	"context"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var displayGlobalDefaults = map[string]any{
	"tool_progress":                  "all",
	"tool_progress_grouping":         "accumulate",
	"show_reasoning":                 false,
	"reasoning_style":                "code",
	"tool_preview_length":            0,
	"streaming":                      nil,
	"interim_assistant_messages":     true,
	"suppress_warning_notifications": false,
	"long_running_notifications":     true,
	"busy_ack_detail":                true,
	"busy_steer_ack_enabled":         true,
	"cleanup_progress":               false,
	"live_status":                    "full",
}

func mergeDisplay(base map[string]any, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

var (
	displayTierHigh = map[string]any{
		"tool_progress": "all", "show_reasoning": false, "tool_preview_length": 40, "streaming": nil,
		"interim_assistant_messages": true, "long_running_notifications": true, "busy_ack_detail": true,
	}
	displayTierMedium = mergeDisplay(displayTierHigh, map[string]any{"tool_progress": "new"})
	displayTierLow    = mergeDisplay(displayTierHigh, map[string]any{
		"tool_progress": "off", "streaming": false, "interim_assistant_messages": false,
		"long_running_notifications": false, "busy_ack_detail": false,
	})
	displayTierMinimal    = mergeDisplay(displayTierLow, map[string]any{"tool_preview_length": 0})
	displayCoreDefaults   = map[string]map[string]any{"webhook": displayTierMinimal, "api_server": mergeDisplay(displayTierHigh, map[string]any{"tool_preview_length": 0})}
	displayTiers          = map[string]map[string]any{"high": displayTierHigh, "medium": displayTierMedium, "low": displayTierLow, "minimal": displayTierMinimal}
	displayTruthy         = map[string]bool{"true": true, "1": true, "yes": true, "on": true}
	displayFalsy          = map[string]bool{"false": true, "0": true, "no": true}
	displayBoolExtraTruth = map[string]bool{"raw": true, "verbose": true}
)

// Registered display tier and overrides of a plugin platform, assigned by the gateway platform registry
var PlatformDisplayLookup func(ctx context.Context, platformKey string) (tier string, overrides map[string]any, found bool)

// Built in defaults of a platform, the core table else the registered plugin tier with its overrides on top
func platformDisplayDefaults(ctx context.Context, platformKey string) map[string]any {
	if d, ok := displayCoreDefaults[platformKey]; ok {
		return d
	}
	if PlatformDisplayLookup == nil {
		return map[string]any{}
	}
	tierName, overrides, failed := "", map[string]any(nil), false
	func() {
		defer func() {
			if recover() != nil {
				failed = true
			}
		}()
		tierName, overrides, _ = PlatformDisplayLookup(ctx, platformKey)
	}()
	if failed {
		return map[string]any{}
	}
	tier := displayTiers[pystr.Lower(tierName)]
	if tier == nil {
		tier = map[string]any{}
	}
	if len(overrides) > 0 {
		return mergeDisplay(tier, overrides)
	}
	return tier
}

// PlatformDisplayDefaults is the built in display defaults of a platform, the core table else its registered tier
func PlatformDisplayDefaults(ctx context.Context, platformKey string) map[string]any {
	return platformDisplayDefaults(ctx, platformKey)
}

// CoreDisplayPlatforms is the platforms whose display defaults the core table declares
func CoreDisplayPlatforms() []string {
	out := make([]string, 0, len(displayCoreDefaults))
	for k := range displayCoreDefaults {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Per platform overrideable display keys, the keys of the global display defaults
func DisplayOverrideableKeys() map[string]bool {
	out := make(map[string]bool, len(displayGlobalDefaults))
	for k := range displayGlobalDefaults {
		out[k] = true
	}
	return out
}

// Display setting from the platform override, the global display key, the platform tier, then the global default
func ResolveDisplaySetting(ctx context.Context, userConfig map[string]any, platformKey, setting string, fallback any) any {
	if configured := configuredDisplayValue(userConfig, platformKey, setting); configured != nil {
		return normaliseDisplay(setting, configured)
	}
	val := platformDisplayDefaults(ctx, platformKey)[setting]
	if val == nil {
		val = displayGlobalDefaults[setting]
	}
	if val == nil {
		return fallback
	}
	return val
}

// First non null operator value of a display setting without tier defaults, display.streaming being CLI only
func configuredDisplayValue(userConfig map[string]any, platformKey, setting string) any {
	displayCfg, ok := userConfig["display"].(map[string]any)
	if !ok {
		return nil
	}
	platforms, _ := displayCfg["platforms"].(map[string]any)
	if platOverrides, ok := platforms[platformKey].(map[string]any); ok && platOverrides[setting] != nil {
		return platOverrides[setting]
	}
	if setting == "tool_progress" {
		if legacy, ok := displayCfg["tool_progress_overrides"].(map[string]any); ok && legacy[platformKey] != nil {
			return legacy[platformKey]
		}
	}
	if setting != "streaming" {
		return displayCfg[setting]
	}
	return nil
}

// Tool progress mode with whether an operator asked for it, YAML beating the legacy env bridge beating tier defaults
func ResolveToolProgress(ctx context.Context, userConfig map[string]any, platformKey, envMode string) (any, bool) {
	if configured := configuredDisplayValue(userConfig, platformKey, "tool_progress"); configured != nil {
		return normaliseDisplay("tool_progress", configured), true
	}
	if envMode != "" {
		return normaliseDisplay("tool_progress", envMode), true
	}
	return ResolveDisplaySetting(ctx, userConfig, platformKey, "tool_progress", nil), false
}

func normTristate(on, off string, choices, extraTruthy map[string]bool) func(any) any {
	return func(value any) any {
		if b, ok := value.(bool); ok {
			if b {
				return on
			}
			return off
		}
		val := pystr.Lower(pystr.Strip(pyrepr.Str(value)))
		if displayFalsy[val] {
			return off
		}
		if displayTruthy[val] || extraTruthy[val] {
			return on
		}
		if choices[val] {
			return val
		}
		return on
	}
}

func normDisplayBool(value any) any {
	if s, ok := value.(string); ok {
		v := pystr.Lower(pystr.Strip(s))
		return displayTruthy[v] || displayBoolExtraTruth[v]
	}
	return pyrepr.Truthy(value)
}

func normLongRunning(value any) any {
	if s, ok := value.(string); ok && pystr.Lower(pystr.Strip(s)) == "generic" {
		return "generic"
	}
	return normDisplayBool(value)
}

// Only an explicit recognized opt in may hide engine diagnostics
func normSuppressWarningNotifications(value any) any {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return displayTruthy[pystr.Lower(pystr.Strip(v))]
	}
	return false
}

func normCleanupProgress(value any) any {
	if s, ok := value.(string); ok {
		return displayTruthy[pystr.Lower(s)]
	}
	return pyrepr.Truthy(value)
}

func normChoice(choices ...string) func(any) any {
	return func(value any) any {
		val := pystr.Lower(pyrepr.Str(value))
		for _, c := range choices {
			if c == val {
				return val
			}
		}
		return choices[0]
	}
}

func normDisplayInt(value any) any {
	n, err := pyint.Of(value)
	if err != nil {
		return 0
	}
	return pyint.Native(n)
}

var displayNormalisers = map[string]func(any) any{
	"tool_progress":                  normTristate("all", "off", map[string]bool{"off": true, "new": true, "all": true, "verbose": true, "log": true}, nil),
	"show_reasoning":                 normDisplayBool,
	"streaming":                      normDisplayBool,
	"interim_assistant_messages":     normDisplayBool,
	"suppress_warning_notifications": normSuppressWarningNotifications,
	"long_running_notifications":     normLongRunning,
	"busy_ack_detail":                normDisplayBool,
	"busy_steer_ack_enabled":         normDisplayBool,
	"thinking_progress":              normDisplayBool,
	"cleanup_progress":               normCleanupProgress,
	"live_status":                    normTristate("full", "off", map[string]bool{"full": true, "verb": true, "off": true}, map[string]bool{"all": true}),
	"tool_progress_grouping":         normChoice("accumulate", "separate"),
	"reasoning_style":                normChoice("code", "blockquote", "subtext"),
	"tool_preview_length":            normDisplayInt,
}

// User supplied display value normalised for its setting, unknown settings passing through
func normaliseDisplay(setting string, value any) any {
	if norm := displayNormalisers[setting]; norm != nil {
		return norm(value)
	}
	return value
}

// Per platform config key of local sessions where local reasoning and footer toggles save
const LocalDisplayKey = "cli"

var localDisplayOverlayKeys = []string{"show_reasoning", "runtime_footer"}

// Copied display block as a local surface resolves it, the cli platform values beating the global ones
func LocalDisplayConfig(display any) map[string]any {
	src, _ := display.(map[string]any)
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	platforms, _ := out["platforms"].(map[string]any)
	local, ok := platforms[LocalDisplayKey].(map[string]any)
	if !ok {
		return out
	}
	for _, key := range localDisplayOverlayKeys {
		value := local[key]
		if value == nil {
			continue
		}
		if vm, ok := value.(map[string]any); ok {
			if base, ok := out[key].(map[string]any); ok {
				value = mergeDisplay(base, vm)
			}
		}
		out[key] = normaliseDisplay(key, value)
	}
	return out
}
