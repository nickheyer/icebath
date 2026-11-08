package agent

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	delegateBlockedTools    = map[string]bool{"delegate_task": true, "clarify": true, "memory": true, "send_message": true, "cronjob_manage": true}
	delegateDefaultToolsets = []string{"terminal", "file", "web"}
)

// Reports canonical MCP toolsets and the registered aliases that target one
func isMCPToolsetName(name string) bool {
	if name == "" {
		return false
	}
	if strings.HasPrefix(name, "mcp-") {
		return true
	}
	target := tools.Default.ToolsetAliasTarget(name)
	return target != "" && strings.HasPrefix(target, "mcp-")
}

func staticToolsetTools(ctx context.Context, name string) []string {
	return tools.Default.ResolveStaticToolset(ctx, tools.ScopeKey(home.Dir(ctx)), name)
}

func isSubset(items []string, of map[string]bool) bool {
	for _, t := range items {
		if !of[t] {
			return false
		}
	}
	return true
}

// Parent toolsets plus every static toolset whose resolved tools the parent already holds
func expandParentToolsets(ctx context.Context, parentToolsets map[string]bool) map[string]bool {
	parentToolNames := map[string]bool{}
	for ts := range parentToolsets {
		if _, ok := tools.StaticToolset(ts); ok {
			for _, t := range staticToolsetTools(ctx, ts) {
				parentToolNames[t] = true
			}
		}
	}
	expanded := map[string]bool{}
	for ts := range parentToolsets {
		expanded[ts] = true
	}
	if len(parentToolNames) > 0 {
		for _, ts := range tools.OrderedStaticToolsetNames() {
			if expanded[ts] {
				continue
			}
			if resolved := staticToolsetTools(ctx, ts); len(resolved) > 0 && isSubset(resolved, parentToolNames) {
				expanded[ts] = true
			}
		}
	}
	return expanded
}

// Drops toolsets whose tools are all blocked plus the delegation and kanban composites
func stripBlockedToolsets(ctx context.Context, toolsets []string) []string {
	blocked := map[string]bool{"delegation": true, "kanban": true}
	for _, name := range tools.OrderedStaticToolsetNames() {
		if resolved := staticToolsetTools(ctx, name); len(resolved) > 0 && isSubset(resolved, delegateBlockedTools) {
			blocked[name] = true
		}
	}
	out := []string{}
	for _, t := range toolsets {
		if !blocked[t] {
			out = append(out, t)
		}
	}
	return out
}

// One tool deny toolsets for a role, sorted, subtracted after composite expansion
func blockedToolsetsForRole(role string) []string {
	blockedNames := map[string]bool{}
	for k := range delegateBlockedTools {
		blockedNames[k] = role != "orchestrator" || k != "delegate_task"
	}
	for k, v := range blockedNames {
		if !v {
			delete(blockedNames, k)
		}
	}
	var out []string
	for _, name := range tools.OrderedStaticToolsetNames() {
		defn, _ := tools.StaticToolset(name)
		if len(defn.Tools) > 0 && isSubset(defn.Tools, blockedNames) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Enabled and disabled toolsets of a child, never more than the parent holds
func resolveChildToolsets(ctx context.Context, parent *AIAgent, toolsets []string, effectiveRole string) ([]string, []string) {
	parentEnabled := parent.enabledToolsets
	parentToolsets := map[string]bool{}
	switch {
	case parentEnabled != nil:
		for _, ts := range parentEnabled {
			parentToolsets[ts] = true
		}
	case parent.validToolNames != nil:
		scope := tools.ScopeKey(home.Dir(ctx))
		for name := range parent.validToolNames {
			if ts := tools.Default.ToolsetForTool(ctx, scope, name); ts != "" {
				parentToolsets[ts] = true
			}
		}
	default:
		for _, ts := range delegateDefaultToolsets {
			parentToolsets[ts] = true
		}
	}
	sortedParent := make([]string, 0, len(parentToolsets))
	for ts := range parentToolsets {
		sortedParent = append(sortedParent, ts)
	}
	sort.Strings(sortedParent)
	var childToolsets []string
	switch {
	case len(toolsets) > 0:
		expanded := expandParentToolsets(ctx, parentToolsets)
		childToolsets = []string{}
		for _, t := range toolsets {
			if expanded[t] {
				childToolsets = append(childToolsets, t)
			}
		}
		if tools.InheritMCPToolsets(ctx) {
			for _, name := range sortedParent {
				if isMCPToolsetName(name) && !slices.Contains(childToolsets, name) {
					childToolsets = append(childToolsets, name)
				}
			}
		}
	case parentEnabled != nil:
		childToolsets = parentEnabled
	default:
		childToolsets = sortedParent
		if len(childToolsets) == 0 {
			childToolsets = delegateDefaultToolsets
		}
	}
	childToolsets = stripBlockedToolsets(ctx, childToolsets)
	inheritedDisabled := []string{}
	if parent.disabledToolsets != nil {
		inheritedDisabled = append(inheritedDisabled, parent.disabledToolsets...)
	}
	if effectiveRole == "orchestrator" {
		inheritedDisabled = slices.DeleteFunc(inheritedDisabled, func(n string) bool { return n == "delegation" })
		if !slices.Contains(childToolsets, "delegation") {
			childToolsets = append(childToolsets, "delegation")
		}
	}
	var disabled []string
	seen := map[string]bool{}
	for _, name := range append(append(inheritedDisabled, blockedToolsetsForRole(effectiveRole)...), "kanban") {
		if !seen[name] {
			seen[name] = true
			disabled = append(disabled, name)
		}
	}
	return childToolsets, disabled
}
