package agent

import (
	"errors"
	"fmt"
	"math"

	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Completion attributes an agent provider hands back with its own transcript rows and tool iteration count
const (
	ProjectedMessagesAttr      = "icebath_projected_messages"
	ProviderToolIterationsAttr = "icebath_provider_tool_iterations"
)

// Appends the provider projected transcript rows and ticks the skill nudge counter, returning the rows spliced
func spliceProviderProjection(a *AIAgent, response any, messages *[]*state.Message) int {
	var rows []map[string]any
	switch projected := state.ToolCallField(response, ProjectedMessagesAttr).(type) {
	case []any:
		for _, m := range projected {
			if row, ok := m.(map[string]any); ok {
				rows = append(rows, row)
			}
		}
	case []map[string]any:
		for _, row := range projected {
			if row != nil {
				rows = append(rows, row)
			}
		}
	}
	for _, row := range rows {
		appendLiveMessage(messages, row)
	}
	if len(rows) > 0 {
		pkgLog().Debug(
			fmt.Sprintf("spliced %d provider-projected transcript row(s) from %s", len(rows), a.provider))
	}
	if iterations := providerToolIterations(state.ToolCallField(response, ProviderToolIterationsAttr)); iterations > 0 {
		if a.itersSinceSkill > math.MaxInt-iterations {
			a.itersSinceSkill = math.MaxInt
		} else {
			a.itersSinceSkill += iterations
		}
	}
	return len(rows)
}

// Positive iteration count of the attribute, the largest int for a count beyond int64, zero for anything else
func providerToolIterations(raw any) int {
	if !anyutil.IsTrue(raw) {
		return 0
	}
	n, err := anyutil.ToInt(raw)
	if (err != nil && !errors.Is(err, anyutil.ErrRange)) || n <= 0 {
		return 0
	}
	return int(min(n, math.MaxInt))
}
