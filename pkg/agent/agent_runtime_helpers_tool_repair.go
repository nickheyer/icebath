package agent

import (
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pydifflib"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Valid tool name for a mismatched one after XML trimming, case, separator, CamelCase, suffix and fuzzy repairs
func (a *AIAgent) repairToolCall(toolName string) (string, bool) {
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
	norm := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(pystr.Lower(s), "-", "_"), " ", "_")
	}
	lowered := pystr.Lower(toolName)
	if valid[lowered] {
		return lowered, true
	}
	normalized := norm(toolName)
	if valid[normalized] {
		return normalized, true
	}
	var cands []string
	add := func(items ...string) {
		for _, c := range items {
			if !slices.Contains(cands, c) {
				cands = append(cands, c)
			}
		}
	}
	add(toolName, lowered, normalized, camelSnake(toolName))
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
		if c != "" && valid[c] {
			return c, true
		}
	}
	names := make([]string, 0, len(valid))
	for name := range valid {
		names = append(names, name)
	}
	slices.Sort(names)
	if matches := pydifflib.GetCloseMatches(lowered, names, 1, 0.7); len(matches) > 0 {
		return matches[0], true
	}
	return "", false
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
