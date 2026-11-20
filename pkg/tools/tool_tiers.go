package tools

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/linktier"
)

// Toolsets, platform bundles and configurator categories outside the core and the tier that carries each
var toolsetTiers = map[string]string{
	"x_search": linktier.Plugins, "image_gen": linktier.Plugins, "video_gen": linktier.Plugins, "tts": linktier.Plugins,
	"stt": linktier.Plugins, "discord": linktier.Plugins, "discord_admin": linktier.Plugins, "yuanbao": linktier.Plugins,
	"feishu_doc": linktier.Plugins, "feishu_drive": linktier.Plugins, "spotify": linktier.Plugins, "a2a": linktier.Plugins,
	"homeassistant": linktier.Plugins, "camofox": linktier.Plugins,
	"icebath-telegram": linktier.Plugins, "icebath-discord": linktier.Plugins, "icebath-whatsapp": linktier.Plugins,
	"icebath-slack": linktier.Plugins, "icebath-signal": linktier.Plugins, "icebath-bluebubbles": linktier.Plugins,
	"icebath-email": linktier.Plugins, "icebath-mattermost": linktier.Plugins, "icebath-matrix": linktier.Plugins,
	"icebath-dingtalk": linktier.Plugins, "icebath-feishu": linktier.Plugins, "icebath-weixin": linktier.Plugins,
	"icebath-qqbot": linktier.Plugins, "icebath-wecom": linktier.Plugins, "icebath-wecom-callback": linktier.Plugins,
	"icebath-yuanbao": linktier.Plugins, "icebath-sms": linktier.Plugins, "icebath-gateway": linktier.Plugins,
	"browser": linktier.Extras, "browser-cdp": linktier.Extras, "browser-use": linktier.Extras, "code_execution": linktier.Extras,
	"computer_use": linktier.Extras, "cronjob": linktier.Extras, "kanban": linktier.Extras, "google_meet": linktier.Extras,
	"icebath-acp": linktier.Extras, "icebath-api-server": linktier.Extras, "icebath-cron": linktier.Extras,
}

// Name prefixes of the tool families bundled plugins register outside the core and the tier of each
var toolPrefixTiers = []struct{ prefix, tier string }{
	{"spotify_", linktier.Plugins}, {"a2a_", linktier.Plugins}, {"ha_", linktier.Plugins}, {"camofox_", linktier.Plugins},
	{"meet_", linktier.Extras}, {"kanban_", linktier.Extras}, {"browser_", linktier.Extras},
}

// Tools of the static toolsets outside the core and the tier of each, platform bundles left out as they carry core tools
var toolTiers = func() map[string]string {
	out := map[string]string{}
	for name, tier := range toolsetTiers {
		if strings.HasPrefix(name, BundlePrefix) {
			continue
		}
		for _, t := range Toolsets[name].Tools {
			out[t] = tier
		}
	}
	return out
}()

// Core tools whose every bundled backend is in a tier outside the core and that tier
var webBackendTools = map[string]string{"web_search": linktier.Plugins, "web_extract": linktier.Plugins}

// Tier of a tool configurator platform, empty for the core ones
func platformTier(key string) string {
	switch key {
	case "cli", "webhook":
		return ""
	case "api_server", "cron":
		return linktier.Extras
	}
	return linktier.Plugins
}

// ToolsetTierRefusal is the line naming the build that carries a toolset this binary leaves out, false for a linked or unknown one
func ToolsetTierRefusal(name string) (string, bool) {
	name = CanonicalToolsetName(strings.TrimSpace(name))
	return linktier.Refusal("the "+name+" toolset", toolsetTiers[name])
}

// ToolTierRefusal is the line naming the build that carries a tool this binary leaves out, false for a linked or unknown one
func ToolTierRefusal(name string) (string, bool) {
	tier := toolTiers[name]
	for _, p := range toolPrefixTiers {
		if tier == "" && strings.HasPrefix(name, p.prefix) {
			tier = p.tier
		}
	}
	return linktier.Refusal("the "+name+" tool", tier)
}

// PlatformTierRefusal is the line naming the build that carries a tool configurator platform this binary leaves out
func PlatformTierRefusal(key string) (string, bool) {
	return linktier.Refusal("the "+key+" platform", platformTier(key))
}

// DropUnlinkedToolsets removes the toolsets, configurator rows, provider categories and platforms of tiers this binary leaves out, run once before any entry
func DropUnlinkedToolsets() {
	unlinked := func(name string) bool {
		_, refused := linktier.Refusal(name, toolsetTiers[name])
		return refused
	}
	toolsetsMu.Lock()
	for name := range Toolsets {
		if unlinked(name) {
			delete(Toolsets, name)
		}
	}
	toolsetsMu.Unlock()
	rows := ConfigurableToolsets[:0:0]
	for _, row := range ConfigurableToolsets {
		if !unlinked(row.Key) {
			rows = append(rows, row)
		}
	}
	ConfigurableToolsets = rows
	for _, key := range append([]string{}, ToolCategories.Keys...) {
		if unlinked(key) {
			ToolCategories.Delete(key)
		}
	}
	if !linktier.Linked(linktier.Plugins) {
		dropBundledWebBackendRows()
		for name := range webBackendTools {
			if d := Default.Get(name); d != nil {
				d.RequiresEnv = nil
			}
		}
	}
	platforms := Platforms[:0:0]
	for _, p := range Platforms {
		if _, refused := PlatformTierRefusal(p.Key); !refused {
			platforms = append(platforms, p)
		}
	}
	Platforms = platforms
}

// Removes the web category rows that select a bundled web backend, the plugins tier carrying every one of them
func dropBundledWebBackendRows() {
	v, _ := ToolCategories.Get("web")
	category, _ := v.(*pyjson.Object)
	if category == nil {
		return
	}
	providers, _ := category.Get("providers")
	list, _ := providers.([]any)
	kept := []any{}
	for _, r := range list {
		if row, ok := r.(*pyjson.Object); ok {
			if _, bundled := row.Get("web_backend"); bundled {
				continue
			}
		}
		kept = append(kept, r)
	}
	category.Set("providers", kept)
}
