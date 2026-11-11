package commands

import (
	"context"
	"sort"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Slash completer the TUI gateway runs over a session's skill and bundle tables and the live sources of the ctx profile
func TUISlashCompleter(ctx context.Context, skillCommands, skillBundles skills.SkillCommands) *SlashCompleter {
	readonlyCfg := func() (config.Tree, error) { return config.LoadReadonly(ctx, home.Dir(ctx), nil) }
	return NewCompleter(CompleterSources{
		Cfg: func() config.Tree {
			cfg, err := readonlyCfg()
			if err != nil {
				return config.Tree{}
			}
			return cfg
		},
		Skills:  func() map[string]SkillInfo { return tuiSkillInfos(skillCommands) },
		Bundles: func() map[string]BundleInfo { return tuiBundleInfos(skillBundles) },
		Skins: func() []NameMeta {
			var rows []NameMeta
			for _, s := range config.ListSkins(ctx) {
				meta := pyrepr.Str(s.Description)
				if !pyrepr.Truthy(s.Description) {
					meta = s.Source
				}
				rows = append(rows, NameMeta{pyrepr.Str(s.Name), meta})
			}
			return rows
		},
		ToolsetRows: func() []ToolsetRow {
			cfg, err := readonlyCfg()
			if err != nil {
				return nil
			}
			enabled := tools.Default.PlatformTools(ctx, "", cfg, "cli", tools.PlatformToolsOptions{IncludeDefaultMCPServers: false})
			var rows []ToolsetRow
			for _, ts := range tools.ConfigurableToolsets {
				rows = append(rows, ToolsetRow{Key: ts.Key, Label: ts.Label, Enabled: enabled[ts.Key]})
			}
			pluginKeys := tools.PluginToolsetKeys(ctx)
			keys := make([]string, 0, len(pluginKeys))
			for k := range pluginKeys {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				rows = append(rows, ToolsetRow{Key: k, Label: "plugin toolset", Enabled: enabled[k]})
			}
			return rows
		},
		ConnectedPlatforms: func() []PlatformHome {
			gw, err := gateway.LoadGatewayConfig(ctx)
			if err != nil || gw == nil {
				return nil
			}
			var out []PlatformHome
			for _, p := range gw.GetConnectedPlatforms(ctx) {
				row := PlatformHome{Name: string(p)}
				if h := gw.GetHomeChannel(p); h != nil && pyrepr.Truthy(h.Name) {
					row.Home = pyrepr.Str(h.Name)
				}
				out = append(out, row)
			}
			return out
		},
	})
}

// Skill command rows in the shape the completer reads
func tuiSkillInfos(found skills.SkillCommands) map[string]SkillInfo {
	out := make(map[string]SkillInfo, len(found))
	for key, info := range found {
		out[key] = SkillInfo{Name: tuiStr(info["name"]), Description: tuiStr(info["description"]), SkillDir: tuiStr(info["skill_dir"]),
			SkillMDPath: tuiStr(info["skill_md_path"]), SkillIdentifier: tuiStr(info["skill_identifier"])}
	}
	return out
}

// Skill bundle rows in the shape the completer reads
func tuiBundleInfos(found skills.SkillCommands) map[string]BundleInfo {
	out := make(map[string]BundleInfo, len(found))
	for key, info := range found {
		var members []string
		if list, ok := info["skills"].([]any); ok {
			for _, m := range list {
				members = append(members, pyrepr.Str(m))
			}
		} else if list, ok := info["skills"].([]string); ok {
			members = append(members, list...)
		}
		out[key] = BundleInfo{Name: tuiStr(info["name"]), Slug: tuiStr(info["slug"]), Description: tuiStr(info["description"]), Skills: members}
	}
	return out
}

// String value of a skill table field, empty for an absent one
func tuiStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}
