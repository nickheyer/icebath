package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One shot fact frozen on the agent at its first tool build, read under the launch policy bound to ctx
func (a *AIAgent) agentIsOneshot(ctx context.Context) bool {
	return tools.AgentIsOneshot(ctx, &a.oneshotSession, a.env)
}

// Tool schemas with delegate_task swapped for its one shot variant without live control parameters
func OneshotToolSchemas(schemas []map[string]any) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(schemas))
	for _, tool := range schemas {
		raw := tool["function"]
		fn, isMap := pyjson.AsObject(raw)
		if !isMap && pyrepr.Truthy(raw) {
			return nil, fmt.Errorf("tool schema function is %s where a mapping was expected", errchain.Kind(raw))
		}
		if !isMap {
			fn = pyjson.NewObject()
		}
		if name, _ := fn.Vals["name"].(string); name != "delegate_task" {
			out = append(out, tool)
			continue
		}
		params, hasParams := pyjson.Dict(fn.Vals["parameters"])
		if !hasParams {
			return nil, errors.New("the delegate_task tool schema has no parameters")
		}
		if _, hasProps := pyjson.Dict(params["properties"]); !hasProps {
			return nil, errors.New("the delegate_task tool schema parameters have no properties")
		}
		variant := tools.OneshotDelegateFunction(fn)
		swapped := make(map[string]any, len(tool))
		for k, v := range tool {
			swapped[k] = v
		}
		swapped["function"] = variant
		out = append(out, swapped)
	}
	return out, nil
}
