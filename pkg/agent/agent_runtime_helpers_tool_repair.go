package agent

import (
	"fmt"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pydifflib"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Offered tool a mismatched name repairs to, never one other than a registered tool the name means
func (a *AIAgent) repairToolCall(toolName string) (string, bool) {
	name, offered := a.resolveToolName(toolName)
	if !offered {
		return "", false
	}
	return name, true
}

// Tool a mismatched name means after XML trimming, alias, case, separator, CamelCase, suffix and fuzzy repairs, offered or registered
func (a *AIAgent) resolveToolName(toolName string) (string, bool) {
	if toolName == "" {
		return "", false
	}
	for _, sep := range []string{`"`, "'", "<", ">"} {
		if idx := strings.Index(toolName, sep); idx > 0 {
			toolName = toolName[:idx]
		}
	}
	if toolName == "" {
		return "", false
	}
	valid := a.validToolNames
	var registered map[string]bool
	loadRegistered := func() map[string]bool {
		if registered == nil {
			registered = tools.RegisteredToolNames(a.toolContext(), a.env)
			for name := range a.deferredToolNames() {
				registered[name] = true
			}
		}
		return registered
	}
	norm := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(pystr.Lower(s), "-", "_"), " ", "_")
	}
	lowered := pystr.Lower(toolName)
	normalized := norm(toolName)
	var cands []string
	add := func(items ...string) {
		for _, c := range items {
			if !slices.Contains(cands, c) {
				cands = append(cands, c)
			}
		}
	}
	add(toolName, tools.CanonicalToolName(toolName), lowered, tools.CanonicalToolName(lowered), normalized, camelSnake(toolName))
	for range 2 {
		var extra []string
		for _, c := range cands {
			if stripped, ok := stripToolSuffix(c); ok && stripped != "" {
				extra = append(extra, stripped, norm(stripped), camelSnake(stripped))
			}
		}
		add(extra...)
	}
	for _, c := range cands {
		if c == "" {
			continue
		}
		if valid[c] {
			return c, true
		}
		if loadRegistered()[c] {
			return c, false
		}
	}
	names := make([]string, 0, len(valid)+len(registered))
	for name := range valid {
		names = append(names, name)
	}
	for name := range loadRegistered() {
		if !valid[name] {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	if matches := pydifflib.GetCloseMatches(lowered, names, 1, 0.7); len(matches) > 0 {
		return matches[0], valid[matches[0]]
	}
	return "", false
}

// Deferred tools this session reaches through tool_call, empty when the request offers every tool directly
func (a *AIAgent) deferredToolNames() map[string]bool {
	if a.env == nil {
		return map[string]bool{}
	}
	return toolSearchScopedNames(a)
}

// Tool result for a call naming no offered tool, saying why when the name means a registered tool
func (a *AIAgent) invalidToolContent(toolName string) (string, bool) {
	target, offered := a.resolveToolName(toolName)
	if target == "" || offered {
		return tools.InvalidToolNameErrorContent(toolName, a.validToolNames), false
	}
	reason := tools.UnofferedToolReason(a.toolContext(), a.env, target, a.deferredToolNames(), a.enabledToolsets, a.disabledToolsets)
	if reason == "" {
		return tools.InvalidToolNameErrorContent(toolName, a.validToolNames), false
	}
	if target == toolName || target == tools.CanonicalToolName(toolName) {
		return reason, true
	}
	return fmt.Sprintf("Tool '%s' does not exist, the nearest tool is '%s'. %s", toolName, target, reason), true
}

// Underscore before every ASCII capital past the first character, then lowercased
func camelSnake(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if i > 0 && s[i] >= 'A' && s[i] <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteByte(s[i])
	}
	return pystr.Lower(b.String())
}

// Name without a trailing tool suffix and its separators, false when no suffix ends it
func stripToolSuffix(s string) (string, bool) {
	lc := pystr.Lower(s)
	for _, sfx := range []string{"_tool", "-tool", "tool"} {
		if strings.HasSuffix(lc, sfx) {
			return pystr.RstripChars(pystr.Slice(s, 0, -len(sfx)), "_-"), true
		}
	}
	return "", false
}
