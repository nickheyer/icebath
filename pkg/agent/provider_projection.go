package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
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
		slog.Default().With("logger", "agent.provider_projection").Debug(
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

// Python int of the iteration attribute or zero, zero again when int raises
func providerToolIterations(raw any) int {
	if !pyrepr.Truthy(raw) {
		return 0
	}
	if num, ok := raw.(json.Number); ok && strings.ContainsAny(num.String(), ".eE") {
		f, err := num.Float64()
		if err != nil {
			return 0
		}
		raw = f
	}
	n, err := pyint.Of(raw)
	if err != nil {
		return 0
	}
	if !n.IsInt64() || n.Int64() > math.MaxInt {
		if n.Sign() > 0 {
			return math.MaxInt
		}
		return 0
	}
	return int(n.Int64())
}
