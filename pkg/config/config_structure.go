package config

import (
	"fmt"
	"sort"
)

// Fields that belong in a providers entry or the model section, never at the root
var customProviderLikeFields = map[string]bool{"base_url": true, "api_key": true, "rate_limit_delay": true, "api_mode": true}

// Structure problems of a loaded config, removedNote explains a removed tool backend or returns empty
func ValidateConfigStructure(cfg map[string]any, removedNote func(section, name string) string) []ConfigIssue {
	var issues []ConfigIssue
	ValidateVoice(cfg, &issues)
	ValidateTimezone(cfg, &issues)
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !(settingsHooks.Known != nil && settingsHooks.Known(key)) && customProviderLikeFields[key] {
			AddIssue(&issues, "warning",
				fmt.Sprintf("Root-level key '%s' looks misplaced — should it be under 'model:' or inside a 'providers' entry?", key),
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
			if first := splitLines(ci.Hint); len(first) > 0 && first[0] != "" {
				lines = append(lines, "    → "+first[0])
			}
		}
	}
	return lines
}
