package agent

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One shot fact frozen on the agent at its first tool build
func (a *AIAgent) agentIsOneshot() bool {
	return tools.AgentIsOneshot(&a.oneshotSession, a.env)
}

// Tool schemas with delegate_task swapped for its one shot variant without live control parameters
func OneshotToolSchemas(schemas []map[string]any) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(schemas))
	for _, tool := range schemas {
		raw := tool["function"]
		fn, isMap := raw.(map[string]any)
		if !isMap && pyrepr.Truthy(raw) {
			return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(raw))
		}
		if name, _ := fn["name"].(string); name != "delegate_task" {
			out = append(out, tool)
			continue
		}
		variant, err := tools.OneshotDelegateFunction(fn)
		if err != nil {
			return nil, err
		}
		swapped := make(map[string]any, len(tool))
		for k, v := range tool {
			swapped[k] = v
		}
		swapped["function"] = variant
		out = append(out, swapped)
	}
	return out, nil
}
