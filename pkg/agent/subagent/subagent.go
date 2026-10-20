// Agents exposed as tools so a parent can delegate a task to a child with its own session
package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

// Builds a fresh child agent for one task
type Factory func(ctx context.Context) (*agent.Agent, error)

// Nesting allowed when a tool names none
const DefaultMaxDepth = 3

const schema = `{"type":"object","properties":{
"prompt":{"type":"string","description":"Complete task for the child agent, with every detail it needs since it starts with an empty transcript"},
"description":{"type":"string","description":"Short label for the task shown to the user"}},
"required":["prompt"],"additionalProperties":false}`

// Tool that runs a child agent to completion and returns its final text
type Tool struct {
	// Name the parent model calls, Agent when empty
	Name        string
	Description string
	Factory     Factory
	// Levels of children allowed below the root, DefaultMaxDepth when zero
	MaxDepth int
	// Budget for one child run, unlimited when zero
	Timeout time.Duration
	spawned atomic.Int64
}

func (t *Tool) name() string {
	if t.Name != "" {
		return t.Name
	}
	return "Agent"
}

func (t *Tool) Definition() protocol.Tool {
	desc := t.Description
	if desc == "" {
		desc = "Delegates a self contained task to a child agent that works with its own tools and transcript and returns only its final answer."
	}
	return protocol.Tool{Name: t.name(), Description: desc, Parameters: json.RawMessage(schema)}
}

func (*Tool) Annotations() tool.Annotations {
	return tool.Annotations{Destructive: true, OpenWorld: true}
}

type depthKey struct{}

// Nesting level of the agent the context belongs to, zero at the root
func Depth(ctx context.Context) int {
	d, _ := ctx.Value(depthKey{}).(int)
	return d
}

func (t *Tool) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	if t.Factory == nil {
		return tool.Error("subagent: no factory configured"), nil
	}
	var in struct {
		Prompt      string `json:"prompt"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return tool.Error("prompt is required"), nil
	}
	limit := t.MaxDepth
	if limit <= 0 {
		limit = DefaultMaxDepth
	}
	depth := Depth(ctx) + 1
	if depth > limit {
		return tool.Errorf("subagent: nesting deeper than %d is not allowed", limit), nil
	}
	child, err := t.Factory(ctx)
	if err != nil {
		return tool.Errorf("subagent: %v", err), nil
	}
	if child == nil {
		return tool.Error("subagent: factory returned no agent"), nil
	}
	if child.Name == "" {
		child.Name = fmt.Sprintf("%s-%d", t.name(), t.spawned.Add(1))
	}
	if in.Description != "" {
		agent.Observe(ctx, agent.Event{Type: agent.EventNote, Text: fmt.Sprintf("%s: %s", child.Name, in.Description)})
	}
	cctx := context.WithValue(ctx, depthKey{}, depth)
	if t.Timeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(cctx, t.Timeout)
		defer cancel()
	}
	resp, err := child.Run(cctx, []protocol.Part{protocol.Text{Text: in.Prompt}}, func(ev agent.Event) { agent.Observe(ctx, ev) })
	if err != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return tool.Errorf("subagent %s timed out after %s", child.Name, t.Timeout), nil
		}
		return tool.Errorf("subagent %s failed: %v", child.Name, err), nil
	}
	text := agent.Text(resp)
	if strings.TrimSpace(text) == "" {
		return tool.Errorf("subagent %s finished without a text answer", child.Name), nil
	}
	return tool.Text(text), nil
}

var _ tool.Tool = (*Tool)(nil)
