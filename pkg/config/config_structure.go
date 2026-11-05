package config

import (
	"fmt"
	"sort"
	"strings"
)

// Fields that belong inside a custom_providers entry rather than at the root
var customProviderLikeFields = map[string]bool{"base_url": true, "api_key": true, "rate_limit_delay": true, "api_mode": true}

type requiredField struct{ name, hint string }

var (
	cpRequiredFields = []requiredField{
		{"name", "Add a name, e.g.: name: my-provider"},
		{"base_url", "Add the API endpoint URL, e.g.: base_url: https://api.example.com/v1"}}
	fbRequiredFields = []requiredField{
		{"provider", "Add: provider: openrouter (or another provider)"},
		{"model", "Add: model: <model-name>"}}
	fbSingleRequiredFields = []requiredField{
		{"provider", "Add: provider: openrouter (or another provider)"},
		{"model", "Add: model: anthropic/claude-sonnet-4 (or another model)"}}
)

const cpListHint = "Change to:\n  custom_providers:\n    - name: my-provider\n      base_url: https://...\n      api_key: ..."

// Warning for every falsy required field of an entry
func requireFields(issues *[]ConfigIssue, entry map[string]any, label string, fields []requiredField, suffix string) {
	for _, f := range fields {
		if !PyTruthy(entry[f.name]) {
			AddIssue(issues, "warning", fmt.Sprintf("%s is missing '%s' field%s", label, f.name, suffix), f.hint)
		}
	}
}

// Issue for each non mapping list item and required field warnings for each mapping item
func validateEntryList(entries []any, label string, issues *[]ConfigIssue, fields []requiredField, severity, message, hint string) {
	for i, entry := range entries {
		if m := AsMap(entry); m != nil {
			requireFields(issues, m, fmt.Sprintf("%s[%d]", label, i), fields, "")
			continue
		}
		msg := strings.NewReplacer("{i}", fmt.Sprint(i), "{type}", PyTypeName(entry)).Replace(message)
		AddIssue(issues, severity, msg, hint)
	}
}

// Errors when custom_providers is not a list and field warnings for each list entry
func validateCustomProviders(cp any, issues *[]ConfigIssue) {
	if m := AsMap(cp); m != nil {
		AddIssue(issues, "error", "custom_providers is a dict — it must be a YAML list (items prefixed with '-')", cpListHint)
		suspicious := map[string]bool{}
		for k := range m {
			if customProviderLikeFields[k] {
				suspicious[k] = true
			}
		}
		if len(suspicious) > 0 {
			AddIssue(issues, "warning", fmt.Sprintf("Root-level keys %s look like custom_providers entry fields", pySortedList(suspicious)),
				"These should be indented under a '- name: ...' list entry, not at root level")
		}
		return
	}
	if list, ok := cp.([]any); ok {
		validateEntryList(list, "custom_providers", issues, cpRequiredFields,
			"warning", "custom_providers[{i}] is not a dict (got {type})", "Each entry should have at minimum: name, base_url")
		return
	}
	AddIssue(issues, "error", fmt.Sprintf("custom_providers is a %s — it must be a YAML list (items prefixed with '-'); "+
		"legacy custom_providers entries are ignored until it is", PyTypeName(cp)), cpListHint)
}

// Errors for a fallback_model that is neither a mapping nor a list and warnings for missing fields
func validateFallbackModel(fb any, issues *[]ConfigIssue) {
	if list, ok := fb.([]any); ok {
		validateEntryList(list, "fallback_model", issues, fbRequiredFields,
			"error", "fallback_model[{i}] should be a dict, got {type}", "Each entry needs provider + model")
		return
	}
	m := AsMap(fb)
	if m == nil {
		AddIssue(issues, "error", fmt.Sprintf("fallback_model should be a dict with 'provider' and 'model', got %s", PyTypeName(fb)),
			"Change to:\n  fallback_model:\n    provider: openrouter\n    model: anthropic/claude-sonnet-4")
		return
	}
	if len(m) > 0 {
		requireFields(issues, m, "fallback_model", fbSingleRequiredFields, " — fallback will be disabled")
	}
}

// Structure problems of a loaded config, removedNote explains a removed tool backend or returns empty
func ValidateConfigStructure(cfg map[string]any, removedNote func(section, name string) string) []ConfigIssue {
	var issues []ConfigIssue
	ValidateVoice(cfg, &issues)
	ValidateTimezone(cfg, &issues)
	cp, hasCP := cfg["custom_providers"]
	fb, hasFB := cfg["fallback_model"]
	if hasCP && cp != nil {
		validateCustomProviders(cp, &issues)
	}
	if hasFB && fb != nil {
		validateFallbackModel(fb, &issues)
	}
	if m := AsMap(cp); m != nil && !hasFB {
		if _, inside := m["fallback_model"]; inside {
			AddIssue(&issues, "error", "fallback_model appears inside custom_providers instead of at root level",
				"Move fallback_model to the top level of config.yaml (no indentation)")
		}
	}
	if PyTruthy(cp) && !PyTruthy(cfg["model"]) {
		AddIssue(&issues, "warning",
			"custom_providers defined but no 'model' section — Icebath won't know which provider to use",
			"Add a model section:\n  model:\n    provider: custom\n    default: your-model-name\n    base_url: https://...")
	}
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !strings.HasPrefix(key, "_") && !KnownRootKey(key) && customProviderLikeFields[key] {
			AddIssue(&issues, "warning",
				fmt.Sprintf("Root-level key '%s' looks misplaced — should it be under 'model:' or inside a 'custom_providers' entry?", key),
				fmt.Sprintf("Move '%s' under the appropriate section", key))
		}
	}
	ValidateWebBackends(cfg, &issues, removedNote)
	ValidateQuotedContainers(cfg, &issues)
	return issues
}

// Severity tagged lines with each first hint line for an unknown provider error
func ConfigStructureIssueLines(issues []ConfigIssue) []string {
	var lines []string
	for _, ci := range issues {
		tag := "WARNING"
		if ci.Severity == "error" {
			tag = "ERROR"
		}
		lines = append(lines, fmt.Sprintf("  [%s] %s", tag, ci.Message))
		if ci.Hint != "" {
			if first := pySplitLines(ci.Hint); len(first) > 0 && first[0] != "" {
				lines = append(lines, "    → "+first[0])
			}
		}
	}
	return lines
}
