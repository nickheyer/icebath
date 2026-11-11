package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Shallow copy of a mapping value, TypeError naming the type for anything that is not a mapping
func pyMappingCopy(v any) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, &PyError{"TypeError", "'" + pyrepr.TypeName(v) + "' object is not a mapping"}
	}
	out := make(map[string]any, len(m))
	for k, val := range m {
		out[k] = val
	}
	return out, nil
}

// Every platform block merged into gwData platforms, gateway.platforms then platforms then gateway.<platform>
func MergePlatformSections(ctx context.Context, yamlCfg map[string]any, gatewayCfg any, gwData map[string]any, also map[string]bool) (map[string]any, error) {
	platformsData := dictSlot(gwData, "platforms")
	merge := func(source any) error {
		src, ok := source.(map[string]any)
		if !ok {
			return nil
		}
		for _, name := range config.MappingKeys(src) {
			raw := src[name]
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			prior, had := platformsData[name]
			existing, ok := prior.(map[string]any)
			if !ok {
				existing = map[string]any{}
			}
			existingExtra, present := existing["extra"]
			if !present {
				existingExtra = map[string]any{}
			}
			mergedExtra, err := pyMappingCopy(existingExtra)
			if err != nil {
				return err
			}
			blockExtra, present := block["extra"]
			if !present {
				blockExtra = map[string]any{}
			}
			overlay, err := pyMappingCopy(blockExtra)
			if err != nil {
				return err
			}
			for k, v := range overlay {
				mergedExtra[k] = v
			}
			if _, ok := block["enabled"]; ok {
				mergedExtra["_enabled_explicit"] = true
			}
			merged := make(map[string]any, len(existing)+len(block))
			for k, v := range existing {
				merged[k] = v
			}
			for k, v := range block {
				merged[k] = v
			}
			if len(mergedExtra) > 0 {
				merged["extra"] = mergedExtra
			}
			platformsData[name] = merged
			if !had {
				config.AppendMappingKey(platformsData, name)
			}
		}
		return nil
	}
	nested, _ := gatewayCfg.(map[string]any)
	if err := merge(nested["platforms"]); err != nil {
		return nil, err
	}
	if err := merge(yamlCfg["platforms"]); err != nil {
		return nil, err
	}
	shorthand := map[string]any{}
	var shorthandOrder []string
	for _, k := range config.MappingKeys(nested) {
		v := nested[k]
		if _, isMap := v.(map[string]any); k != "platforms" && isMap && (also[k] || isPlatformName(ctx, k)) {
			shorthand[k] = v
			shorthandOrder = append(shorthandOrder, k)
		}
	}
	config.SetMappingKeys(shorthand, shorthandOrder)
	return platformsData, merge(shorthand)
}

// Whether a key names a built in or registered platform
func isPlatformName(ctx context.Context, key any) bool {
	_, err := ParsePlatform(ctx, key)
	return err == nil
}

// Platform block and whether it is top level, a nested block standing in when no top level mapping exists
func platformSection(yamlCfg map[string]any, name string, gatewayPlatforms any) (any, bool) {
	section := yamlCfg[name]
	if _, toplevel := section.(map[string]any); toplevel {
		return section, true
	}
	for _, src := range []any{gatewayPlatforms, yamlCfg["platforms"]} {
		if m, ok := src.(map[string]any); ok {
			if block, ok := m[name].(map[string]any); ok {
				return block, false
			}
		}
	}
	return section, false
}

// Mapping copy with string keys, any other value unchanged
func strKeyed(value any) any {
	if m, ok := value.(map[string]any); ok {
		out := make(map[string]any, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	return value
}

type sharedKey struct {
	key       string
	only      map[Platform]bool
	transform string
}

var (
	onlyTelegram     = map[Platform]bool{PlatformTelegram: true}
	onlyDiscordSlack = map[Platform]bool{PlatformDiscord: true, PlatformSlack: true}
)

// Keys copied verbatim into extra for every platform
func plainKeys(keys ...string) []sharedKey {
	out := make([]sharedKey, len(keys))
	for i, k := range keys {
		out[i] = sharedKey{key: k}
	}
	return out
}

// Shared platform keys bridged into extra with their platform filter and transform
var sharedKeys = func() []sharedKey {
	var out []sharedKey
	out = append(out, sharedKey{"unauthorized_dm_behavior", nil, "dm"}, sharedKey{"notice_delivery", nil, "notice"})
	out = append(out, plainKeys("reply_prefix", "reply_in_thread", "cron_continuable_surface", "require_mention", "send_read_receipts")...)
	out = append(out, sharedKey{"allowed_chats", onlyTelegram, ""}, sharedKey{"group_allowed_chats", onlyTelegram, ""},
		sharedKey{"allowed_topics", onlyTelegram, ""})
	out = append(out, plainKeys("free_response_channels", "mention_patterns", "exclusive_bot_mentions")...)
	out = append(out, sharedKey{"observe_unmentioned_group_messages", onlyTelegram, ""})
	out = append(out, plainKeys("dm_policy", "allow_from", "allow_admin_from", "user_allowed_commands",
		"group_policy", "group_allow_from", "group_allow_admin_from", "group_user_allowed_commands")...)
	out = append(out, sharedKey{"channel_skill_bindings", onlyDiscordSlack, ""}, sharedKey{"channel_prompts", nil, "strkeyed"})
	return append(out, plainKeys("gateway_restart_notification", "typing_indicator", "typing_status_text")...)
}()

// Normalized choice whose fallback may be any value
func normalizeChoiceAny(value any, choices map[string]bool, def any) any {
	const miss = "\x00miss"
	if n := normalizeChoice(value, choices, miss); n != miss {
		return n
	}
	return def
}

// Shared keys of a platform block, a root block also promoting its adapter keys into extra
func bridgedKeys(plat Platform, platformCfg, gwData map[string]any, rootBlock bool) map[string]any {
	bridged := map[string]any{}
	if rootBlock {
		for k, v := range platformCfg {
			if !PlatformTypedKeys[k] {
				bridged[k] = v
			}
		}
		explicit, ok := platformCfg["extra"]
		if !ok {
			explicit = map[string]any{}
		}
		for k, v := range coerceDict(explicit) {
			bridged[k] = v
		}
	}
	for _, sk := range sharedKeys {
		v, ok := platformCfg[sk.key]
		if !ok || (sk.only != nil && !sk.only[plat]) {
			continue
		}
		switch sk.transform {
		case "dm":
			def, ok := gwData["unauthorized_dm_behavior"]
			if !ok {
				def = "pair"
			}
			bridged[sk.key] = normalizeChoiceAny(v, UnauthorizedDMBehaviors, def)
		case "notice":
			bridged[sk.key] = normalizeChoice(v, map[string]bool{"public": true, "private": true}, "public")
		case "strkeyed":
			bridged[sk.key] = strKeyed(v)
		default:
			bridged[sk.key] = v
		}
	}
	return bridged
}

// Built in platforms plus registered plugin platforms, all registered plugin entries when entries is nil
func sharedLoopTargets(ctx context.Context, registry *PlatformRegistry, entries []*PlatformEntry) []Platform {
	targets := PlatformMembers()
	if entries == nil && registry != nil {
		entries = registry.PluginEntries(ctx)
	}
	seen := map[Platform]bool{}
	for _, t := range targets {
		seen[t] = true
	}
	for _, e := range entries {
		if p, err := ParsePlatform(ctx, e.Name); err == nil && !seen[p] {
			seen[p] = true
			targets = append(targets, p)
		}
	}
	return targets
}

// The extra each platform authored under platforms.<plat>.extra, copied before either copy site runs
func snapshotAuthoredExtra(platformsData map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for name, raw := range platformsData {
		plat, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		copied := map[string]any{}
		for k, v := range coerceDict(plat["extra"]) {
			copied[k] = v
		}
		out[name] = copied
	}
	return out
}

// Drops authored extra keys an administrator pinned and restores managed extra values, returning hook pins
func applyManagedExtra(ctx context.Context, authored map[string]map[string]any, managed, platformsData map[string]any) (map[string]map[string]any, error) {
	if len(managed) == 0 {
		return map[string]map[string]any{}, nil
	}
	managedPlatforms, err := MergePlatformSections(ctx, managed, managed["gateway"], map[string]any{}, nil)
	if err != nil {
		return nil, err
	}
	hookPins := map[string]map[string]any{}
	for name, extra := range authored {
		root := coerceDict(managed[name])
		nested := coerceDict(managedPlatforms[name])
		managedExtra := coerceDict(nested["extra"])
		pinned := map[string]bool{}
		for k := range root {
			pinned[k] = true
		}
		for k := range coerceDict(root["extra"]) {
			pinned[k] = true
		}
		for k := range nested {
			pinned[k] = true
		}
		for k := range pinned {
			if _, keep := managedExtra[k]; !keep {
				delete(extra, k)
			}
		}
		for k, v := range config.DeepMerge(extra, managedExtra) {
			extra[k] = v
		}
		if len(managedExtra) > 0 {
			destination := dictSlot(dictSlot(platformsData, name), "extra")
			for k, v := range config.DeepMerge(destination, managedExtra) {
				destination[k] = v
			}
			pins := map[string]any{}
			for k := range managedExtra {
				if !PlatformTypedKeys[k] {
					pins[k] = extra[k]
				}
			}
			hookPins[name] = pins
		}
	}
	return hookPins, nil
}

// Effective platform block with the authored extra overlaid, one warning per differing platform key
func authoredWins(extra, block map[string]any, platName string, toplevel bool, warned map[[2]string]bool) map[string]any {
	if warned == nil {
		warned = map[[2]string]bool{}
	}
	where := "platforms." + platName
	if toplevel {
		where = fmt.Sprintf("the top-level '%s:' block", platName)
	}
	overlayOnto := func(source map[string]any) map[string]any {
		out := make(map[string]any, len(source))
		for k, v := range source {
			out[k] = v
		}
		for k, sv := range source {
			value, ok := extra[k]
			if !ok {
				continue
			}
			if !pyrepr.Equal(sv, value) && !warned[[2]string{platName, k}] {
				warned[[2]string{platName, k}] = true
				slog.Warn(fmt.Sprintf("%s set in both %s and platforms.%s.extra; platforms.%s.extra took precedence", k, where, platName, platName))
			}
			out[k] = value
		}
		return out
	}
	effective := overlayOnto(block)
	if sub, ok := block["extra"].(map[string]any); ok {
		effective["extra"] = overlayOnto(sub)
		for k := range sub {
			if v, ok := extra[k]; ok && !PlatformTypedKeys[k] {
				effective[k] = v
			}
		}
	}
	return effective
}

// Copies shared keys of each platform's YAML section into extra, enabled only from a top level block
func BridgePlatformSharedKeys(yamlCfg map[string]any, gatewayPlatforms any, gwData, platformsData map[string]any, targets []Platform,
	warned map[[2]string]bool, authored map[string]map[string]any) {
	if authored == nil {
		authored = snapshotAuthoredExtra(platformsData)
	}
	for _, plat := range targets {
		if plat == PlatformLocal {
			continue
		}
		section, toplevel := platformSection(yamlCfg, string(plat), gatewayPlatforms)
		platformCfg, ok := section.(map[string]any)
		if !ok {
			continue
		}
		effective := authoredWins(authored[string(plat)], platformCfg, string(plat), toplevel, warned)
		bridged := bridgedKeys(plat, effective, gwData, toplevel)
		_, hasOverrides := platformCfg["channel_overrides"]
		if overrides, ok := platformCfg["channel_overrides"].(map[string]any); ok {
			platData := dictSlot(platformsData, string(plat))
			dictSlot(platData, "extra")
			kept := map[string]any{}
			for cid, ov := range overrides {
				if _, isMap := ov.(map[string]any); isMap {
					kept[cid] = ov
				}
			}
			platData["channel_overrides"] = kept
		}
		_, hasEnabled := platformCfg["enabled"]
		enabledExplicit := toplevel && hasEnabled
		if len(bridged) == 0 && !enabledExplicit && !hasOverrides {
			continue
		}
		platData := dictSlot(platformsData, string(plat))
		extra := dictSlot(platData, "extra")
		if enabledExplicit {
			platData["enabled"] = platformCfg["enabled"]
			extra["_enabled_explicit"] = true
		}
		for k, v := range bridged {
			extra[k] = v
		}
	}
}

// Runs each plugin's YAML to env config bridge on its effective block, merging seeded values into extra
func ApplyPluginYAMLHooks(ctx context.Context, yamlCfg map[string]any, gatewayPlatforms any, platformsData map[string]any, registry *PlatformRegistry,
	warned map[[2]string]bool, authored, managedExtra map[string]map[string]any, entries []*PlatformEntry) {
	if registry == nil {
		return
	}
	if authored == nil {
		authored = snapshotAuthoredExtra(platformsData)
	}
	if entries == nil {
		entries = registry.AllEntries(ctx)
	}
	for _, entry := range entries {
		if entry.ApplyYAMLConfigFn == nil {
			continue
		}
		section, toplevel := platformSection(yamlCfg, entry.Name, gatewayPlatforms)
		platformCfg, ok := section.(map[string]any)
		if !ok {
			continue
		}
		effective := authoredWins(authored[entry.Name], platformCfg, entry.Name, toplevel, warned)
		for k, v := range managedExtra[entry.Name] {
			effective[k] = v
		}
		seeded, err := entry.ApplyYAMLConfigFn(yamlCfg, effective)
		if err != nil {
			slog.Debug(fmt.Sprintf("apply_yaml_config_fn for %s raised: %s", entry.Name, err))
			continue
		}
		if len(seeded) > 0 {
			extra := dictSlot(dictSlot(platformsData, entry.Name), "extra")
			for k, v := range seeded {
				extra[k] = v
			}
		}
	}
}

// User config.yaml with env references expanded and the managed overlay applied, an absent file an empty layer
func ReadYAMLLayers(ctx context.Context, h string) (map[string]any, error) {
	path := filepath.Join(h, "config.yaml")
	var doc any = map[string]any{}
	exists, err := config.ConfigExists(ctx, path)
	if err != nil {
		return nil, err
	}
	if exists {
		read, err := config.ReadConfigDoc(ctx, path)
		if err != nil {
			return nil, err
		}
		if pyrepr.Truthy(read) {
			doc = read
		}
	}
	m, ok := config.Expand(doc, os.LookupEnv).(map[string]any)
	if !ok {
		return nil, &PyError{"AttributeError", "'" + pyrepr.TypeName(doc) + "' object has no attribute 'get'"}
	}
	return config.ApplyManagedOverlay(m), nil
}

// Overlays the YAML layers onto gwData of any shape, a non mapping failing as the attribute lookup does
func LoadYAMLLayerAny(ctx context.Context, h string, gwData any) error {
	if m, ok := gwData.(map[string]any); ok {
		return LoadYAMLLayer(ctx, h, m)
	}
	yamlCfg, err := ReadYAMLLayers(ctx, h)
	if err != nil || len(yamlCfg) == 0 {
		return err
	}
	return &PyError{"AttributeError", "'" + pyrepr.TypeName(gwData) + "' object has no attribute 'get'"}
}

// Overlays the YAML layers onto gwData in place, bridging shared keys and plugin hooks for configured platforms
func LoadYAMLLayer(ctx context.Context, h string, gwData map[string]any) error {
	yamlCfg, err := ReadYAMLLayers(ctx, h)
	if err != nil {
		return err
	}
	if len(yamlCfg) == 0 {
		return nil
	}
	gatewaySection := yamlCfg["gateway"]
	BridgeToplevelKeys(yamlCfg, gatewaySection, gwData)
	var gatewayPlatforms any
	if m, ok := gatewaySection.(map[string]any); ok {
		gatewayPlatforms = m["platforms"]
	}
	platformsData, err := MergePlatformSections(ctx, yamlCfg, gatewaySection, gwData, nil)
	if err != nil {
		return err
	}
	plugins.DiscoverPlugins(ctx, false)
	registry := Registry
	names := config.MappingKeys(platformsData)
	entries := registry.ConfiguredEntries(ctx, ActivationPredicate(ctx, yamlCfg, names, h, registry))
	pluginEntries := []*PlatformEntry{}
	for _, e := range entries {
		if e.Source == "plugin" {
			pluginEntries = append(pluginEntries, e)
		}
	}
	targets := sharedLoopTargets(ctx, registry, pluginEntries)
	scratch, err := MergePlatformSections(ctx, yamlCfg, gatewaySection, map[string]any{}, nil)
	if err != nil {
		return err
	}
	authored := snapshotAuthoredExtra(scratch)
	managedExtra, err := applyManagedExtra(ctx, authored, config.ApplyManagedOverlay(map[string]any{}), platformsData)
	if err != nil {
		return err
	}
	warned := map[[2]string]bool{}
	BridgePlatformSharedKeys(yamlCfg, gatewayPlatforms, gwData, platformsData, targets, warned, authored)
	ApplyPluginYAMLHooks(ctx, yamlCfg, gatewayPlatforms, platformsData, registry, warned, authored, managedExtra, entries)
	BridgeCoreEnvSettings(ctx, yamlCfg, platformsData)
	return nil
}

// Gateway config of an explicit local YAML file, refused when the config backend has no file tooling
func LoadGatewayConfigFile(ctx context.Context, path string) (*GatewayConfig, error) {
	if err := config.RequireFileTooling(ctx, "gateway --config <file>"); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(raw) {
		return nil, &PyError{"UnicodeDecodeError", "'utf-8' codec can't decode " + path}
	}
	doc, err := config.FastSafeLoad(raw)
	if err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(doc) {
		doc = map[string]any{}
	}
	return GatewayConfigFromDict(ctx, doc)
}
