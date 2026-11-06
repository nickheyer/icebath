package agent

import (
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// CLIFamilySources are the sources resume treats as CLI history, the interactive cli and finite oneshot runs
var CLIFamilySources = map[string]bool{"cli": true, provider.OneshotSource: true}

// DaybreakSources are the owner's local surfaces, the only sources that carry OpenAI Daybreak
var DaybreakSources = unionSources(provider.UITransportSources, "cli")

// AttendedSources are the sources where a person reads the turn as it runs
var AttendedSources = unionSources(provider.UITransportSources, "cli", "acp")

// Returns a new set holding every member of base plus the extra labels
func unionSources(base map[string]bool, extra ...string) map[string]bool {
	out := make(map[string]bool, len(base)+len(extra))
	for k, v := range base {
		if v {
			out[k] = true
		}
	}
	for _, k := range extra {
		out[k] = true
	}
	return out
}

// CreateSurface maps desktop to the gui session.create label and leaves every other source unchanged
func CreateSurface(source string) string {
	if source == "desktop" {
		return "gui"
	}
	return source
}

// IsAttended reports whether a person reads the agent's turns, false for subagents, library callers and finite turns
func IsAttended(a *AIAgent) bool {
	if a == nil || a.platform == "" || a.platform == "subagent" {
		return false
	}
	if tools.FiniteTurnRequired != nil {
		if required, known := tools.FiniteTurnRequired(a.env); known && required {
			return false
		}
	}
	return AttendedSources[provider.SessionSourceFor(a.env, a.platform)]
}
