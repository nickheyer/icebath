package commands

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Slash completer the TUI gateway runs over a session's skill and bundle tables and the live sources of the ctx profile
func TUISlashCompleter(ctx context.Context, skillCommands, skillBundles skills.SkillCommands) *SlashCompleter {
	readonlyCfg := func() (config.Tree, error) { return config.LoadReadonly(ctx, home.Dir(ctx), nil) }
	return NewCompleter(CompleterSources{
		Ctx: ctx,
		Cfg: func() config.Tree {
			cfg, err := readonlyCfg()
			if err != nil {
				return config.Tree{}
			}
			return cfg
		},
		Skills:             func() map[string]SkillInfo { return tuiSkillInfos(skillCommands) },
		Bundles:            func() map[string]BundleInfo { return tuiBundleInfos(skillBundles) },
		Skins:              func() []NameMeta { return LiveSkinRows(ctx) },
		ToolsetRows:        func() []ToolsetRow { return LiveToolsetRows(ctx) },
		ConnectedPlatforms: func() []PlatformHome { return LiveConnectedPlatforms(ctx) },
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
