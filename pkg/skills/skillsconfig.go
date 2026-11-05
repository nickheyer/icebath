package skills

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Scope over the process home with its loaded config
func processScope() (*Scope, error) {
	h := home.Process()
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return nil, err
	}
	return NewScope(h, cfg), nil
}

// Config variables declared by every enabled platform compatible skill of the process home
func DiscoverAllSkillConfigVars() ([]SkillConfigVar, error) {
	s, err := processScope()
	if err != nil {
		return nil, err
	}
	return s.DiscoverAllSkillConfigVars(), nil
}

// Skill config declarations and their resolved values for config show
func SkillSettingsForShow() ([]config.SkillConfigVar, map[string]any, error) {
	s, err := processScope()
	if err != nil {
		return nil, nil, err
	}
	found := s.DiscoverAllSkillConfigVars()
	out := make([]config.SkillConfigVar, 0, len(found))
	for _, v := range found {
		out = append(out, config.SkillConfigVar{Key: v.Key, Skill: v.Skill, Description: v.Description, Prompt: v.Prompt, Default: v.Default})
	}
	return out, s.ResolveSkillConfigValues(found), nil
}

// Platforms the skills configurator offers, the api server excluded
func skillsConfigPlatforms() []struct{ key, label string } {
	var out []struct{ key, label string }
	for _, p := range tools.Platforms {
		if p.Key != "api_server" {
			out = append(out, struct{ key, label string }{p.Key, p.Info.Label})
		}
	}
	return out
}

// Label of a configurable platform, empty when unknown
func skillsConfigPlatformLabel(key string) (string, bool) {
	for _, p := range skillsConfigPlatforms() {
		if p.key == key {
			return p.label, true
		}
	}
	return "", false
}

// Skill names of a config value, null empty and a bare scalar one item
func normalizeSkillNames(v any) map[string]bool {
	out := map[string]bool{}
	switch t := v.(type) {
	case nil:
		return out
	case string:
		if s := strings.TrimSpace(t); s != "" {
			out[s] = true
		}
		return out
	case []any:
		for _, item := range t {
			if s := strings.TrimSpace(pyStrValue(item)); s != "" {
				out[s] = true
			}
		}
	case map[string]any:
		for k := range t {
			if s := strings.TrimSpace(k); s != "" {
				out[s] = true
			}
		}
	}
	return out
}

// Disabled skill names, the global list unioned with the platform list when a platform is given
func GetDisabledSkills(cfg map[string]any, platform string) map[string]bool {
	skillsCfg, ok := cfg["skills"].(map[string]any)
	if !ok {
		return map[string]bool{}
	}
	disabled := normalizeSkillNames(skillsCfg["disabled"])
	if platform != "" {
		if pd, ok := skillsCfg["platform_disabled"].(map[string]any); ok {
			if v, ok := pd[platform]; ok && v != nil {
				for n := range normalizeSkillNames(v) {
					disabled[n] = true
				}
			}
		}
	}
	for n := range EssentialSkills {
		delete(disabled, n)
	}
	return disabled
}

// Sorted names of a set
func sortedSet(set map[string]bool) []any {
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]any, len(names))
	for i, n := range names {
		out[i] = n
	}
	return out
}

// Persists disabled skill names globally or for one platform, essential skills dropped
func SaveDisabledSkills(ctx context.Context, h string, cfg map[string]any, disabled map[string]bool, platform string) error {
	kept := map[string]bool{}
	for n := range disabled {
		if !EssentialSkills[n] {
			kept[n] = true
		}
	}
	skillsCfg, ok := cfg["skills"].(map[string]any)
	if _, present := cfg["skills"]; !present {
		skillsCfg, ok = map[string]any{}, true
		cfg["skills"] = skillsCfg
	}
	if !ok {
		return fmt.Errorf("'%T' object does not support item assignment", cfg["skills"])
	}
	if platform == "" {
		skillsCfg["disabled"] = sortedSet(kept)
	} else {
		pd, ok := skillsCfg["platform_disabled"].(map[string]any)
		if _, present := skillsCfg["platform_disabled"]; !present {
			pd, ok = map[string]any{}, true
			skillsCfg["platform_disabled"] = pd
		}
		if !ok {
			return fmt.Errorf("'%T' object does not support item assignment", skillsCfg["platform_disabled"])
		}
		pd[platform] = sortedSet(kept)
	}
	return config.SaveConfig(ctx, h, cfg, config.SaveOptions{})
}

// Sorted unique categories with uncategorized standing in for none
func skillCategories(skills []map[string]any) []string {
	set := map[string]bool{}
	for _, s := range skills {
		set[categoryOrUncategorized(s)] = true
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Category of a listed skill, uncategorized when it has none
func categoryOrUncategorized(s map[string]any) string {
	if c := fmString(s["category"]); c != "" {
		return c
	}
	return "uncategorized"
}

// Asks which platform to configure, empty meaning global
func selectSkillsPlatform(c *cliui.Console) string {
	type option struct{ key, label string }
	options := []option{{"global", "All platforms (global default)"}}
	for _, p := range skillsConfigPlatforms() {
		options = append(options, option{p.key, p.label})
	}
	fmt.Fprintln(c.Out)
	fmt.Fprintln(c.Out, c.Color("  Configure skills for:", cliui.Bold))
	for i, o := range options {
		fmt.Fprintf(c.Out, "  %d. %s\n", i+1, o.label)
	}
	fmt.Fprintln(c.Out)
	raw, err := c.Input(c.Color("  Select [1]: ", cliui.Yellow))
	if err != nil {
		return ""
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	idx := n - 1
	if idx >= 0 && idx < len(options) && options[idx].key != "global" {
		return options[idx].key
	}
	return ""
}

// Toggles every skill of a category at once through a category checklist
func toggleByCategory(ctx context.Context, c *cliui.Console, skills []map[string]any, disabled map[string]bool) (map[string]bool, error) {
	cats := skillCategories(skills)
	catSkills := make([]map[string]bool, len(cats))
	labels := make([]string, len(cats))
	pre := map[int]bool{}
	for i, cat := range cats {
		names := map[string]bool{}
		for _, s := range skills {
			if categoryOrUncategorized(s) == cat {
				names[fmString(s["name"])] = true
			}
		}
		catSkills[i] = names
		labels[i] = fmt.Sprintf("%s (%d skills)", cat, len(names))
		for n := range names {
			if !disabled[n] {
				pre[i] = true
				break
			}
		}
	}
	chosen, err := c.CursesChecklist(ctx, "Categories — toggle entire categories", labels, pre, pre, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for n := range disabled {
		out[n] = true
	}
	for i, names := range catSkills {
		for n := range names {
			if chosen[i] {
				delete(out, n)
			} else {
				out[n] = true
			}
		}
	}
	return out, nil
}

// Interactive enable and disable of installed skills globally or per platform
func SkillsCommand(ctx context.Context, c *cliui.Console, h string) error {
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return err
	}
	scope := NewScope(h, cfg)
	skills := scope.FindAllSkills(true)
	if len(skills) == 0 {
		fmt.Fprintln(c.Out, c.Color("  No skills installed.", cliui.Dim))
		return nil
	}
	platform := selectSkillsPlatform(c)
	platformLabel := "All platforms"
	if platform != "" {
		if l, ok := skillsConfigPlatformLabel(platform); ok {
			platformLabel = l
		}
	}
	fmt.Fprintln(c.Out)
	fmt.Fprintln(c.Out, c.Color("  Configure for: "+platformLabel, cliui.Dim))
	fmt.Fprintln(c.Out)
	fmt.Fprintln(c.Out, "  1. Toggle individual skills")
	fmt.Fprintln(c.Out, "  2. Toggle by category")
	fmt.Fprintln(c.Out)
	mode, err := c.Input(c.Color("  Select [1]: ", cliui.Yellow))
	if err != nil {
		return nil
	}
	if mode = strings.TrimSpace(mode); mode == "" {
		mode = "1"
	}
	raw := map[string]any(cfg)
	disabled := GetDisabledSkills(raw, platform)
	var newDisabled map[string]bool
	if mode == "2" {
		if newDisabled, err = toggleByCategory(ctx, c, skills, disabled); err != nil {
			return err
		}
	} else {
		labels := make([]string, len(skills))
		pre := map[int]bool{}
		for i, s := range skills {
			labels[i] = fmt.Sprintf("%s  (%s)  —  %s", fmString(s["name"]), categoryOrUncategorized(s), runeHead(fmString(s["description"]), 55))
			if !disabled[fmString(s["name"])] {
				pre[i] = true
			}
		}
		chosen, err := c.CursesChecklist(ctx, "Skills for "+platformLabel, labels, pre, pre, nil)
		if err != nil {
			return err
		}
		newDisabled = map[string]bool{}
		for i, s := range skills {
			if !chosen[i] {
				newDisabled[fmString(s["name"])] = true
			}
		}
	}
	if sameSet(newDisabled, disabled) {
		fmt.Fprintln(c.Out, c.Color("  No changes.", cliui.Dim))
		return nil
	}
	if err := SaveDisabledSkills(ctx, h, raw, newDisabled, platform); err != nil {
		return err
	}
	enabled := len(skills) - len(newDisabled)
	fmt.Fprintln(c.Out, c.Color(fmt.Sprintf("✓ Saved: %d enabled, %d disabled (%s).", enabled, len(newDisabled), platformLabel), cliui.Green))
	return nil
}

// Reports whether two name sets hold the same names
func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
