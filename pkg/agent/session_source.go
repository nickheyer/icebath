package agent

import (
	"github.com/nickheyer/icebath/pkg/provider"
)

// CLIFamilySources are the sources resume treats as CLI history, the interactive cli and finite oneshot runs
var CLIFamilySources = map[string]bool{"cli": true, provider.OneshotSource: true}

// DaybreakSources are the owner's local surfaces, the only sources that carry OpenAI Daybreak
var DaybreakSources = unionSources(provider.UITransportSources, "cli")

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
