package provider

import (
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Lowercased agent reasoning effort of a config, none for a disabled dict form and empty when unset
func currentReasoningEffort(cfg map[string]any) string {
	agentCfg, ok := cfg["agent"].(map[string]any)
	if !ok {
		return ""
	}
	effort := agentCfg["reasoning_effort"]
	if m, isDict := effort.(map[string]any); isDict {
		parsed := config.ParseReasoningEffort(m)
		if enabled, set := parsed["enabled"]; set && enabled == false {
			effort = "none"
		} else {
			effort = parsed["effort"]
		}
	}
	return pystr.Lower(pystr.Strip(pyOrStr(effort)))
}

// Writes the effort under agent reasoning_effort, a missing or non mapping agent section replaced
func setReasoningEffort(cfg map[string]any, effort string) {
	ensureDictSection(cfg, "agent")["reasoning_effort"] = effort
}
