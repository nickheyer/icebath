package config

import (
	"regexp"
	"strings"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Reasoning effort levels a bare string may name
var ValidReasoningEfforts = []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// Mapping view of a decoded config value, false for anything that is not a dict
func reasoningDict(v any) (map[string]any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return t, true
	case Tree:
		return map[string]any(t), true
	}
	return nil, false
}

// Reasoning config of a level, nil for empty or unknown input and enabled false for a disable spelling
func ParseReasoningEffort(effort any) map[string]any {
	if effort == nil || effort == true {
		return nil
	}
	if m, ok := reasoningDict(effort); ok {
		if enabled, has := m["enabled"]; has && enabled == false {
			return map[string]any{"enabled": false}
		}
		level := ""
		if v := m["effort"]; anyutil.IsTrue(v) {
			level = strings.TrimSpace(anyutil.String(v))
		}
		if level == "" {
			return nil
		}
		return map[string]any{"enabled": true, "effort": level}
	}
	text := strings.ToLower(strings.TrimSpace(anyutil.String(effort)))
	switch text {
	case "none", "false", "disabled":
		return map[string]any{"enabled": false}
	}
	for _, level := range ValidReasoningEfforts {
		if text == level {
			return map[string]any{"enabled": true, "effort": text}
		}
	}
	return nil
}

var (
	digitDashDigit = regexp.MustCompile(`(\p{Nd})-(\p{Nd})`)
	digitDotDigit  = regexp.MustCompile(`(\p{Nd})\.(\p{Nd})`)
)

// Spelling variants of a model id for tolerant override matching, exact first and deduplicated in order
func canonicalModelVariants(model string) []string {
	seen := map[string]bool{}
	var variants []string
	add := func(values ...string) {
		for _, v := range values {
			if v != "" && !seen[v] {
				seen[v] = true
				variants = append(variants, v)
			}
		}
	}
	dashToDot := func(s string) string { return digitDashDigit.ReplaceAllString(s, "$1.$2") }
	dotToDash := func(s string) string { return digitDotDigit.ReplaceAllString(s, "$1-$2") }
	addWithDerivatives := func(s string) {
		dashed, dotted := strings.ReplaceAll(s, ".", "-"), strings.ReplaceAll(s, "-", ".")
		add(s, dashed, dotted, dashToDot(s), dotToDash(s), dashToDot(dashed), dotToDash(dotted))
	}
	addWithDerivatives(model)
	parts := strings.Split(model, "/")
	if len(parts) >= 2 {
		addWithDerivatives(parts[len(parts)-1])
	}
	if len(parts) >= 3 {
		addWithDerivatives(strings.Join(parts[1:], "/"))
	}
	providers := []string{"anthropic", "openai", "google", "openrouter", "groq", "mistral",
		"xai", "cohere", "perplexity", "together", "fireworks", "deepseek"}
	for _, v := range append([]string(nil), variants...) {
		if !strings.Contains(v, "/") {
			for _, p := range providers {
				add(p + "/" + v)
			}
		}
	}
	aggregators := []string{"openrouter", "opencode", "fireworks", "groq", "together"}
	for _, v := range append([]string(nil), variants...) {
		if strings.Count(v, "/") == 1 {
			for _, a := range aggregators {
				add(a + "/" + v)
			}
		}
	}
	return variants
}

// Per model reasoning override with spelling tolerance, prefixed keys matched in reverse, nil when none parses
func ResolvePerModelReasoningEffort(model string, overrides any) map[string]any {
	m, ok := reasoningDict(overrides)
	if !ok || len(m) == 0 || model == "" {
		return nil
	}
	variants := canonicalModelVariants(model)
	for _, v := range variants {
		if raw, has := m[v]; has {
			if result := ParseReasoningEffort(raw); result != nil {
				return result
			}
		}
	}
	variantSet := make(map[string]bool, len(variants))
	for _, v := range variants {
		variantSet[v] = true
	}
	for _, key := range MappingKeys(m) {
		if !strings.Contains(key, "/") {
			continue
		}
		parts := strings.Split(key, "/")
		forms := canonicalModelVariants(parts[len(parts)-1])
		if len(parts) >= 3 {
			forms = append(forms, canonicalModelVariants(strings.Join(parts[1:], "/"))...)
		}
		for _, form := range forms {
			if variantSet[form] {
				if result := ParseReasoningEffort(m[key]); result != nil {
					return result
				}
				break
			}
		}
	}
	return nil
}

// Per model provider routing entry matched with the override spelling tolerance, empty when none matches
func ResolvePerModelProviderRouting(model string, models any) map[string]any {
	m, ok := reasoningDict(models)
	if model == "" || !ok {
		return map[string]any{}
	}
	for _, v := range canonicalModelVariants(model) {
		if entry, ok := reasoningDict(m[v]); ok {
			return entry
		}
	}
	return map[string]any{}
}
