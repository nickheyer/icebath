package tools

import (
	"fmt"
	"maps"
	"strconv"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/setupflow"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Whether platform_toolsets holds an explicitly saved list for the platform, a list literal string counting
func PlatformToolsetsExplicitlySaved(cfg config.Tree, platform string) bool {
	_, explicit := coercePlatformToolsetsValue(cfg.Sub("platform_toolsets")[platform], platform)
	return explicit
}

// Python index of a list of length n, negative counting from the end, false when out of range
func pyListIndex(n, i int) (int, bool) {
	if i < 0 {
		i += n
	}
	return i, i >= 0 && i < n
}

// Display label of a built in platform, the KeyError of an unknown key otherwise
func platformLabel(pkey string) (string, error) {
	info, ok := PlatformInfoFor(pkey)
	if !ok {
		return "", fmt.Errorf("unknown platform %s", pkey)
	}
	return info.Label, nil
}

// Provider category of a toolset, nil when the toolset has none
func categoryOf(tsKey string) *jsonutil.Object {
	v, ok := ToolCategories.Get(tsKey)
	if !ok || !anyutil.IsTrue(v) {
		return nil
	}
	cat, _ := v.(*jsonutil.Object)
	return cat
}

// Seams over the key check, the checklist and the per toolset setup that tests patch
var (
	toolsetHasKeysFn       func(s *ToolsSetup, tsKey string, cfg config.Tree) (bool, error)
	promptToolsetChecklist func(s *ToolsSetup, platformLabel string, enabled map[string]bool, platform string) (map[string]bool, error)
	configureToolset       func(s *ToolsSetup, tsKey string, cfg config.Tree, reconfigure bool) error
)

func init() {
	toolsetHasKeysFn = (*ToolsSetup).toolsetHasKeysImpl
	promptToolsetChecklist = (*ToolsSetup).promptToolsetChecklistImpl
	configureToolset = (*ToolsSetup).configureToolsetImpl
}

// Whether a toolset's required keys are configured, through the seam provider rows and the web routers share
func (s *ToolsSetup) toolsetHasKeys(tsKey string, cfg config.Tree) (bool, error) {
	return toolsetHasKeysFn(s, tsKey, cfg)
}

// Whether a toolset's credentials are configured, a keyless provider counting
func (s *ToolsSetup) toolsetHasKeysImpl(tsKey string, cfg config.Tree) (bool, error) {
	if cfg == nil {
		var err error
		if cfg, err = toolsConfigLoad(s); err != nil {
			return false, err
		}
	}
	if tsKey == "vision" {
		ready, err := resolveVisionProviderClient(s)
		return err == nil && ready, nil
	}
	if cat := categoryOf(tsKey); cat != nil {
		providers, err := s.VisibleProviders(cat)
		if err != nil {
			return false, err
		}
		for _, p := range providers {
			ready, err := providerEnvReady(s.ctx(), s.home(), p)
			if err != nil {
				return false, err
			}
			if ready {
				return true, nil
			}
		}
		return false, nil
	}
	for _, req := range ToolsetEnvRequirements[tsKey] {
		v, err := s.getEnvValue(req[0])
		if err != nil {
			return false, err
		}
		if v == "" {
			return false, nil
		}
	}
	return true, nil
}

// Multi select of the platform's configurable toolsets with a no key marker and an estimated token status line
func (s *ToolsSetup) promptToolsetChecklistImpl(platformLabel string, enabled map[string]bool, platform string) (map[string]bool, error) {
	ctx := s.ctx()
	toolTokens, err := estimateToolTokens(ctx)
	if err != nil {
		return nil, err
	}
	var effective []ConfigurableToolset
	for _, ts := range EffectiveConfigurableToolsets(ctx) {
		if ToolsetAllowedForPlatform(ts.Key, platform) && !ConfigOnlyToolsets[ts.Key] {
			effective = append(effective, ts)
		}
	}
	labels := make([]string, len(effective))
	items := make([]setupflow.Item, len(effective))
	for i, ts := range effective {
		has, err := s.toolsetHasKeys(ts.Key, nil)
		if err != nil {
			return nil, err
		}
		marker := ""
		if !has && isConfigurableToolset(ts.Key) {
			marker = "  [no API key]"
		}
		labels[i] = ts.Label + "  (" + ts.Description + ")" + marker
		items[i] = setupflow.Item{Title: ts.Label, Detail: ts.Description + marker}
		if len(toolTokens) > 0 {
			items[i].Tools = Default.ResolveToolset(ctx, "", ts.Key)
		}
	}
	pre := map[int]bool{}
	for i, ts := range effective {
		if enabled[ts.Key] {
			pre[i] = true
		}
	}
	chosen, err := s.toolsetChecklist("Tools for "+platformLabel, labels, items, pre, toolTokens)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for i := range chosen {
		j, ok := pyListIndex(len(effective), i)
		if !ok {
			return nil, fmt.Errorf("toolset choice %d is past the end of the list", i)
		}
		out[effective[j].Key] = true
	}
	return out, nil
}

// Picked toolset rows, the asker with the tools estimate or the curses list with its token status line
func (s *ToolsSetup) toolsetChecklist(title string, labels []string, items []setupflow.Item, pre map[int]bool, toolTokens map[string]int) (map[int]bool, error) {
	if s.Ask != nil {
		var tokens map[string]int
		if len(toolTokens) > 0 {
			tokens = toolTokens
		}
		picked, err := s.Ask.Check(title, items, pre, tokens)
		if err != nil {
			return nil, err
		}
		chosen := map[int]bool{}
		for i, on := range picked {
			if on {
				chosen[i] = true
			}
		}
		return chosen, nil
	}
	var statusFn func(map[int]bool) string
	if len(toolTokens) > 0 {
		statusFn = func(chosen map[int]bool) string {
			all := map[string]bool{}
			for idx := range chosen {
				for _, t := range items[idx].Tools {
					all[t] = true
				}
			}
			total := 0
			for name := range all {
				total += toolTokens[name]
			}
			if total >= 1000 {
				return "Est. tool context: ~" + strconv.FormatFloat(float64(total)/1000, 'f', 1, 64) + "k tokens"
			}
			return "Est. tool context: ~" + strconv.Itoa(total) + " tokens"
		}
	}
	return toolsChecklist(s, title, labels, pre, maps.Clone(pre), statusFn)
}

// Provider selection and keys for a category toolset, plain env var prompts for the rest
func (s *ToolsSetup) configureToolsetImpl(tsKey string, cfg config.Tree, reconfigure bool) error {
	if cat := categoryOf(tsKey); cat != nil {
		return s.configureToolCategory(tsKey, cat, cfg, reconfigure)
	}
	return s.configureSimpleRequirements(tsKey, reconfigure)
}

// Lets the user pick a configured or enabled tool and redo its provider or key setup
func (s *ToolsSetup) reconfigureTool(cfg config.Tree) error {
	var keys, labels []string
	for _, ts := range EffectiveConfigurableToolsets(s.ctx()) {
		if !isConfigurableToolset(ts.Key) {
			continue
		}
		has, err := s.toolsetHasKeys(ts.Key, cfg)
		if err != nil {
			return err
		}
		if has || s.toolsetEnabledForReconfigure(ts.Key, cfg) {
			keys, labels = append(keys, ts.Key), append(labels, ts.Label)
		}
	}
	if len(keys) == 0 {
		s.Con.PrintInfo("No configured tools to reconfigure.")
		return nil
	}
	choices := append(labels, "Cancel")
	idx, err := s.promptChoice("  Which tool would you like to reconfigure?", choices, len(choices)-1)
	if err != nil {
		return err
	}
	if idx >= len(keys) {
		return nil
	}
	j, ok := pyListIndex(len(keys), idx)
	if !ok {
		return fmt.Errorf("tool choice %d is past the end of the list", idx)
	}
	if err := configureToolset(s, keys[j], cfg, true); err != nil {
		return err
	}
	return toolsConfigSave(s, cfg)
}

// Whether a toolset is on for any platform it may run on, a failing platform skipped
func (s *ToolsSetup) toolsetEnabledForReconfigure(tsKey string, cfg config.Tree) bool {
	for _, p := range Platforms {
		if !ToolsetAllowedForPlatform(tsKey, p.Key) {
			continue
		}
		if enabled, ok := s.platformToolsSafely(cfg, p.Key); ok && enabled[tsKey] {
			return true
		}
	}
	return false
}

// Current platform tools with a resolver failure reported as false
func (s *ToolsSetup) platformToolsSafely(cfg config.Tree, pkey string) (enabled map[string]bool, ok bool) {
	defer func() {
		if recover() != nil {
			enabled, ok = nil, false
		}
	}()
	return s.currentPlatformTools(cfg, pkey), true
}

// Prints a green plus line per added toolset and a red minus line per removed one
func (s *ToolsSetup) printToolsetDiff(added, removed map[string]bool, indent string) {
	for _, ts := range sortedSet(added) {
		s.printColor(indent+"+ "+toolsetLabel(s.ctx(), ts), cliui.Green)
	}
	for _, ts := range sortedSet(removed) {
		s.printColor(indent+"- "+toolsetLabel(s.ctx(), ts), cliui.Red)
	}
}

// Selected configurable toolsets still missing provider or key setup, sorted
func (s *ToolsSetup) toolsetsNeedingSetup(newEnabled map[string]bool, cfg config.Tree) ([]string, error) {
	var out []string
	for _, ts := range sortedSet(newEnabled) {
		if !isConfigurableToolset(ts) {
			continue
		}
		needs, err := s.toolsetNeedsConfigurationPrompt(ts, cfg)
		if err != nil {
			return nil, err
		}
		if needs {
			out = append(out, ts)
		}
	}
	return out, nil
}

// Configures newly enabled toolsets needing keys that the selected pass did not handle
func (s *ToolsSetup) configureNewlyAdded(added, already map[string]bool, cfg config.Tree) error {
	fresh := map[string]bool{}
	for k := range added {
		if !already[k] {
			fresh[k] = true
		}
	}
	keys, err := s.toolsetsNeedingSetup(fresh, cfg)
	if err != nil {
		return err
	}
	for _, ts := range keys {
		if err := configureToolset(s, ts, cfg, false); err != nil {
			return err
		}
	}
	return nil
}

// Menu row of one platform with its enabled and total toolset counts
func (s *ToolsSetup) platformMenuLabel(cfg config.Tree, pkey string) (string, error) {
	count := len(s.currentPlatformTools(cfg, pkey))
	total := len(EffectiveConfigurableToolsets(s.ctx()))
	label, err := platformLabel(pkey)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Configure %s  (%d/%d enabled)", label, count, total), nil
}

// The tools --summary listing of enabled toolsets per platform
func (s *ToolsSetup) printToolsSummary(cfg config.Tree, enabledPlatforms []string) error {
	ctx := s.ctx()
	total := len(EffectiveConfigurableToolsets(ctx))
	s.printColor("󰆧 Tool Summary", cliui.Cyan, cliui.Bold)
	s.println("")
	summary, err := Default.PlatformToolsetSummary(ctx, "", s.home(), cfg, append([]string{}, enabledPlatforms...))
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, entry := range summary {
		if seen[entry.Platform] {
			continue
		}
		seen[entry.Platform] = true
		label, err := platformLabel(entry.Platform)
		if err != nil {
			return err
		}
		s.println(s.color("  "+label, cliui.Bold) + s.color(fmt.Sprintf("  (%d/%d)", len(entry.Enabled), total), cliui.Dim))
		for _, ts := range sortedSet(entry.Enabled) {
			s.printColor("    ✓ "+toolsetLabel(ctx, ts), cliui.Green)
		}
		if len(entry.Enabled) == 0 {
			s.printColor("    (none enabled)", cliui.Dim)
		}
	}
	s.println("")
	return nil
}

// Announces then configures each listed toolset
func (s *ToolsSetup) configureList(toConfigure []string, cfg config.Tree, selected bool) error {
	if len(toConfigure) == 0 {
		return nil
	}
	s.println("")
	what := "tool(s)"
	if selected {
		what = "selected tool(s)"
	}
	s.printColor(fmt.Sprintf("  Configuring %d %s:", len(toConfigure), what), cliui.Yellow)
	for _, ts := range toConfigure {
		s.printColor("    • "+toolsetLabel(s.ctx(), ts), cliui.Dim)
	}
	s.printColor("  You can skip any tool you don't need right now.", cliui.Dim)
	s.println("")
	for _, ts := range toConfigure {
		if err := configureToolset(s, ts, cfg, false); err != nil {
			return err
		}
	}
	return nil
}

// Writes a platform selection into the config and saves it, the save platform tools
func (s *ToolsSetup) savePlatformToolsConfig(cfg config.Tree, platform string, enabled map[string]bool) error {
	ctx := s.ctx()
	if err := Default.SavePlatformTools(ctx, "", cfg, platform, enabled, PluginToolsetKeys(ctx)); err != nil {
		return err
	}
	return toolsConfigSave(s, cfg)
}

// Fresh install pass, one checklist per platform then setup for every configurable selection
func (s *ToolsSetup) firstInstallFlow(cfg config.Tree, enabledPlatforms []string) error {
	for _, pkey := range enabledPlatforms {
		label, err := platformLabel(pkey)
		if err != nil {
			return err
		}
		current := s.currentPlatformTools(cfg, pkey)
		offered := map[string]bool{}
		for k := range current {
			if !DefaultOffToolsets[k] {
				offered[k] = true
			}
		}
		newEnabled, err := promptToolsetChecklist(s, label, offered, pkey)
		if err != nil {
			return err
		}
		added, removed := checklistDiff(s.ctx(), newEnabled, current, pkey)
		s.printToolsetDiff(added, removed, "  ")
		var toConfigure []string
		for _, ts := range sortedSet(newEnabled) {
			if isConfigurableToolset(ts) {
				toConfigure = append(toConfigure, ts)
			}
		}
		if err := s.configureList(toConfigure, cfg, false); err != nil {
			return err
		}
		if err := s.savePlatformToolsConfig(cfg, pkey, newEnabled); err != nil {
			return err
		}
		if err := toolsConfigSave(s, cfg); err != nil {
			return err
		}
		s.printColor("  ✓ Saved "+label+" tool configuration", cliui.Green)
		s.println("")
	}
	return nil
}

// Toolsets a platform resolves to without the implicit MCP servers
func (s *ToolsSetup) currentPlatformTools(cfg config.Tree, pkey string) map[string]bool {
	return Default.PlatformTools(s.ctx(), "", cfg, pkey, PlatformToolsOptions{})
}

// Prints the diff, configures new toolsets the selected pass skipped and writes the platform list
func (s *ToolsSetup) applyPlatformChecklist(cfg config.Tree, pkey string, newEnabled, prev, already map[string]bool, indent string, header bool) error {
	added, removed := checklistDiff(s.ctx(), newEnabled, prev, pkey)
	if header && (len(added) > 0 || len(removed) > 0) {
		label, err := platformLabel(pkey)
		if err != nil {
			return err
		}
		s.printColor("  "+label+":", cliui.Dim)
	}
	s.printToolsetDiff(added, removed, indent)
	if err := s.configureNewlyAdded(added, already, cfg); err != nil {
		return err
	}
	return s.savePlatformToolsConfig(cfg, pkey, newEnabled)
}

// Checklist, key setup and save for one platform or every platform at once, true when the config was saved
func (s *ToolsSetup) configurePlatforms(cfg config.Tree, platformKeys []string, allPlatforms bool) (bool, error) {
	label := "All platforms"
	if !allPlatforms {
		if len(platformKeys) == 0 {
			return false, fmt.Errorf("no platform is configured to choose from")
		}
		var err error
		if label, err = platformLabel(platformKeys[0]); err != nil {
			return false, err
		}
	}
	current := map[string]map[string]bool{}
	allCurrent := map[string]bool{}
	for _, pk := range platformKeys {
		current[pk] = s.currentPlatformTools(cfg, pk)
		maps.Copy(allCurrent, current[pk])
	}
	newEnabled, err := promptToolsetChecklist(s, label, allCurrent, "cli")
	if err != nil {
		return false, err
	}
	selected, err := s.toolsetsNeedingSetup(newEnabled, cfg)
	if err != nil {
		return false, err
	}
	if err := s.configureList(selected, cfg, true); err != nil {
		return false, err
	}
	if maps.Equal(newEnabled, allCurrent) && len(selected) == 0 {
		if allPlatforms {
			s.printColor("  No changes", cliui.Dim)
		} else {
			s.printColor("  No changes to "+label, cliui.Dim)
		}
		return false, nil
	}
	already := map[string]bool{}
	for _, ts := range selected {
		already[ts] = true
	}
	indent := "  "
	if allPlatforms {
		indent = "    "
	}
	for _, pk := range platformKeys {
		prev := current[pk]
		if allPlatforms {
			prev = s.currentPlatformTools(cfg, pk)
		}
		if err := s.applyPlatformChecklist(cfg, pk, newEnabled, prev, already, indent, allPlatforms); err != nil {
			return false, err
		}
	}
	if err := toolsConfigSave(s, cfg); err != nil {
		return false, err
	}
	if allPlatforms {
		s.printColor("  ✓ Saved configuration for all platforms", cliui.Green)
	} else {
		s.printColor("  ✓ Saved "+label+" configuration", cliui.Green)
	}
	return true, nil
}
