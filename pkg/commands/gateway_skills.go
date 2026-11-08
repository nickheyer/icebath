package commands

import (
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Resolved path, or the path itself when resolution fails as pathlib resolve(strict=False) keeps going
func resolvedOrSelf(p string) string {
	if r, err := pypath.Resolve(p); err == nil {
		return r
	}
	return p
}

// Skills eligible as gateway slash commands for a platform, hermes_cli/commands_platforms.py _iter_gateway_skills
func IterGatewaySkills(scope *skills.Scope, platform string) []GatewaySkill {
	disabled := scope.DisabledSkillNames(platform)
	skillsDir := resolvedOrSelf(scope.SkillsDir())
	hubDir := resolvedOrSelf(pypath.PathJoin(scope.SkillsDir(), ".hub"))
	roots := []string{skillsDir}
	for _, getter := range []func() []string{scope.ExternalSkillsDirs, scope.ProjectSkillsDirs} {
		for _, d := range getter() {
			roots = append(roots, resolvedOrSelf(d))
		}
	}
	cmds := scope.GetSkillCommands()
	keys := make([]string, 0, len(cmds))
	for k := range cmds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []GatewaySkill
	for _, key := range keys {
		info := cmds[key]
		skillPath := pyrepr.Str(info["skill_md_path"])
		if info["skill_md_path"] == nil || skillPath == "" {
			continue
		}
		sp := resolvedOrSelf(skillPath)
		if _, under := pypath.PathRelativeTo(sp, hubDir); under {
			continue
		}
		root := ""
		for _, r := range roots {
			if _, under := pypath.PathRelativeTo(sp, r); under {
				root = r
				break
			}
		}
		name := pyrepr.Str(info["name"])
		if info["name"] == nil {
			name = ""
		}
		if root == "" || disabled[name] {
			continue
		}
		rel, _ := pypath.PathRelativeTo(pypath.PathParent(sp), root)
		var parts []string
		if rel != "" && rel != "." {
			parts = strings.Split(rel, "/")
		}
		desc := pyrepr.Str(info["description"])
		if info["description"] == nil {
			desc = ""
		}
		out = append(out, GatewaySkill{Key: key, Description: desc, RelParts: parts})
	}
	return out
}

// SkillSource over the home's skills scope with the given config
func HomeSkillSource(h string, cfg config.Tree) SkillSource {
	scope := skills.NewScope(h, cfg)
	return func(platform string) []GatewaySkill { return IterGatewaySkills(scope, platform) }
}

// GatewaySkillEntries is the exported collectGatewaySkillEntries, names and descriptions with the hidden count
func GatewaySkillEntries(r *Registry, platform string, source SkillSource, maxSlots int, reserved map[string]bool,
	descLimit int, sanitize func(string) string) ([]NameDesc, int) {
	entries, hidden := collectGatewaySkillEntries(r, platform, source, maxSlots, reserved, descLimit, sanitize)
	out := make([]NameDesc, 0, len(entries))
	for _, e := range entries {
		out = append(out, NameDesc{Name: e.name, Description: e.desc})
	}
	return out, hidden
}
