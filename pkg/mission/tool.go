package mission

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

// Name the model calls to keep its plan current
const ToolName = "Plan"

const schema = `{"type":"object","properties":{
"steps":{"type":"array","items":{"type":"object","properties":{
 "text":{"type":"string","description":"What the step achieves"},
 "status":{"type":"string","enum":["pending","active","done","skipped"]}},"required":["text"],"additionalProperties":false},
 "description":"Complete plan in order, replacing the previous one, at most one step active"},
"note":{"type":"string","description":"Short status note shown beside the plan"}},
"required":["steps"],"additionalProperties":false}`

// Tool that replaces the plan of the mission the calling run serves
type Tool struct {
	Store *Store
}

func (Tool) Definition() protocol.Tool {
	return protocol.Tool{
		Name:        ToolName,
		Description: "Replaces the plan of the current mission with the full ordered list of steps and their statuses. Call it when starting work and whenever a step changes state, marking exactly one step active while you work on it.",
		Parameters:  json.RawMessage(schema),
	}
}

func (Tool) Annotations() tool.Annotations {
	return tool.Annotations{Idempotent: true}
}

func (t Tool) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	if t.Store == nil {
		return tool.Error("mission: no store configured"), nil
	}
	id := IDFrom(ctx)
	if id == "" {
		return tool.Error("no mission is attached to this run"), nil
	}
	var in struct {
		Steps []Step `json:"steps"`
		Note  string `json:"note"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	if len(in.Steps) == 0 {
		return tool.Error("steps are required"), nil
	}
	active := 0
	for _, s := range in.Steps {
		if s.Status == StepActive {
			active++
		}
	}
	if active > 1 {
		return tool.Error("only one step may be active"), nil
	}
	m, err := t.Store.SetPlan(id, in.Steps)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if in.Note != "" {
		if m, err = t.Store.SetNote(id, in.Note); err != nil {
			return tool.Error(err.Error()), nil
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Plan updated, %d%% done\n", int(m.Progress()*100+0.5))
	for i, s := range m.Plan {
		fmt.Fprintf(&sb, "%d. [%s] %s\n", i+1, s.Status, s.Text)
	}
	return tool.Text(strings.TrimRight(sb.String(), "\n")).With(m), nil
}

var _ tool.Tool = Tool{}
