package gateway

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Value of key in a decoded JSON object of either representation
func jsonObjectGet(v any, key string) (any, bool, bool) {
	switch d := v.(type) {
	case map[string]any:
		got, ok := d[key]
		return got, ok, true
	case *pyjson.Object:
		got, ok := d.Get(key)
		return got, ok, true
	}
	return nil, false, false
}

// Explicit platforms.relay enabled false in the profile YAML, read with the gateway loader merge rules
func RelayExplicitlyDisabled(ctx context.Context) (bool, error) {
	cfg, err := ReadYAMLLayers(ctx, home.Dir(ctx))
	if err != nil || cfg == nil {
		return false, nil
	}
	gw, ok := cfg["gateway"].(map[string]any)
	if !ok {
		gw = map[string]any{}
	}
	platforms, err := MergePlatformSections(ctx, cfg, gw, map[string]any{}, nil)
	if err != nil {
		return false, err
	}
	BridgePlatformSharedKeys(cfg, gw["platforms"], map[string]any{}, platforms, []Platform{PlatformRelay}, nil, nil)
	block, ok := platforms["relay"].(map[string]any)
	if !ok {
		return false, nil
	}
	extra, has := block["extra"]
	if !has {
		extra = map[string]any{}
	}
	explicit, _, isDict := jsonObjectGet(extra, "_enabled_explicit")
	if !isDict {
		return false, &PyError{"AttributeError", "'" + pyrepr.TypeName(extra) + "' object has no attribute 'get'"}
	}
	if !pyrepr.Truthy(explicit) {
		return false, nil
	}
	pc, err := PlatformConfigFromDict(ctx, block)
	if err != nil {
		return false, err
	}
	return !pc.Enabled, nil
}

// Parsed GATEWAY_RELAY_BOT_IDS, empty when unset or malformed
func relayBotIDsMap() any {
	raw := pystr.Strip(os.Getenv("GATEWAY_RELAY_BOT_IDS"))
	if raw == "" {
		return map[string]any{}
	}
	parsed, err := pyjson.Loads(raw)
	if err != nil {
		slog.Warn("GATEWAY_RELAY_BOT_IDS is not valid JSON; treating as empty")
		return map[string]any{}
	}
	if _, _, ok := jsonObjectGet(parsed, ""); ok {
		return parsed
	}
	return map[string]any{}
}

// Ordered platform and bot id pairs the relay fronts, the generic relay pair when nothing is set
func RelayPlatformIdentities() [][2]string {
	var platforms []string
	for _, p := range strings.Split(os.Getenv("GATEWAY_RELAY_PLATFORMS"), ",") {
		if s := pystr.Strip(p); s != "" {
			platforms = append(platforms, s)
		}
	}
	if len(platforms) == 0 {
		return [][2]string{{"relay", ""}}
	}
	ids := relayBotIDsMap()
	out := make([][2]string, 0, len(platforms))
	for _, platform := range platforms {
		entry, _, _ := jsonObjectGet(ids, platform)
		if !pyrepr.Truthy(entry) {
			entry = map[string]any{}
		}
		botID := ""
		if v, has, isDict := jsonObjectGet(entry, "botId"); isDict {
			if !has {
				v = ""
			}
			botID = pystr.Strip(pyrepr.Str(v))
		}
		out = append(out, [2]string{platform, botID})
	}
	return out
}

// Logical platforms the relay fronts minus the generic relay fallback, empty when relay is explicitly disabled
func RelayFrontedPlatformsChecked(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	disabled, err := RelayExplicitlyDisabled(ctx)
	if err != nil || disabled {
		return out, err
	}
	for _, id := range RelayPlatformIdentities() {
		if id[0] != "relay" {
			out[id[0]] = true
		}
	}
	return out, nil
}

// Fronted platforms raising a malformed relay config the way the uncaught Python exception does
func RelayFrontedPlatforms(ctx context.Context) map[string]bool {
	out, err := RelayFrontedPlatformsChecked(ctx)
	if err != nil {
		panic(err)
	}
	return out
}
