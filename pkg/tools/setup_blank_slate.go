package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Writes the Blank Slate toolset state, only file, terminal, vision and skills left on through both suppression layers
func BlankSlateMinimalToolsets(ctx context.Context, cfg config.Tree) error {
	keep := map[string]bool{"file": true, "terminal": true, "vision": true, "skills": true}
	platformToolsets, ok := pyDictOf(cfg["platform_toolsets"])
	if _, present := cfg["platform_toolsets"]; !present {
		platformToolsets, ok = map[string]any{}, true
		cfg["platform_toolsets"] = platformToolsets
	}
	if !ok {
		return fmt.Errorf("TypeError: '%s' object does not support item assignment", pyrepr.TypeName(cfg["platform_toolsets"]))
	}
	kept := []any{}
	for _, k := range sortedSet(keep) {
		kept = append(kept, k)
	}
	platformToolsets["cli"] = kept
	allKeys := configurableKeys()
	for k := range PluginToolsetKeys(ctx) {
		allKeys[k] = true
	}
	var configurableToolSets []map[string]bool
	for _, c := range ConfigurableToolsets {
		set := map[string]bool{}
		for _, t := range Default.ResolveToolset(ctx, "", c.Key) {
			set[t] = true
		}
		configurableToolSets = append(configurableToolSets, set)
	}
	for _, k := range staticToolsetNames() {
		spec, _ := staticToolset(k)
		if strings.HasPrefix(k, "icebath-") || strings.HasPrefix(k, "hermes-") || len(spec.Includes) > 0 || spec.Posture {
			continue
		}
		if !allKeys[k] && subsetOfAnyToolSet(Default.ResolveToolset(ctx, "", k), configurableToolSets) {
			continue
		}
		allKeys[k] = true
	}
	var disabled []any
	for _, k := range sortedSet(allKeys) {
		if !keep[k] {
			disabled = append(disabled, k)
		}
	}
	if len(disabled) == 0 {
		return nil
	}
	agent, ok := pyDictOf(cfg["agent"])
	if _, present := cfg["agent"]; !present {
		agent, ok = map[string]any{}, true
		cfg["agent"] = agent
	}
	if !ok {
		toolsConfigLogger().Debug(fmt.Sprintf("blank-slate disabled_toolsets computation skipped: '%s' object does not support item assignment", pyrepr.TypeName(cfg["agent"])))
		return nil
	}
	agent["disabled_toolsets"] = disabled
	return nil
}

// Whether a non empty tool list sits wholly inside one of the toolset tool sets
func subsetOfAnyToolSet(tools []string, sets []map[string]bool) bool {
	if len(tools) == 0 {
		return false
	}
	for _, set := range sets {
		inside := true
		for _, t := range tools {
			if !set[t] {
				inside = false
				break
			}
		}
		if inside {
			return true
		}
	}
	return false
}
