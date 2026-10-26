package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

// Builds a fresh child agent for one task
type Factory func(ctx context.Context) (*Agent, error)

// Nesting allowed when a tool names none
const DefaultMaxDepth = 3

// Finished children remembered before the oldest is forgotten
const maxChildren = 64

// Name the parent model calls to delegate
const SubagentTool = "Agent"

const subagentSchema = `{"type":"object","properties":{
"prompt":{"type":"string","description":"Complete task for the child agent, with every detail it needs since it starts with an empty transcript"},
"description":{"type":"string","description":"Short label for the task shown to the user"}},
"required":["prompt"],"additionalProperties":false}`

// One child the tool spawned, live or finished
type Child struct {
	Name        string
	Description string
	Parent      string
	Depth       int
	CallID      string
	Agent       *Agent
	Started     time.Time
	Finished    time.Time
	Running     bool
	Err         error
	Answer      string
	cancel      context.CancelFunc
}

// Tool that runs a child agent to completion and returns its final text
type Subagents struct {
	Factory Factory
	// Levels of children allowed below the root, DefaultMaxDepth when zero
	MaxDepth int
	// Budget for one child run, unlimited when zero
	Timeout time.Duration
	// Lets several children of one round run at the same time
	Parallel bool
	mu       sync.Mutex
	spawned  int
	children map[string]*Child
	order    []string
	used     map[string]bool
}

func (t *Subagents) Definition() protocol.Tool {
	return protocol.Tool{Name: SubagentTool, Description: "Delegates a self contained task to a child agent that works with its own tools and transcript and returns only its final answer.", Parameters: json.RawMessage(subagentSchema)}
}

// Destructive and open world always, and read only when children may run side by side
func (t *Subagents) Annotations() tool.Annotations {
	return tool.Annotations{ReadOnly: t.Parallel, Destructive: true, OpenWorld: true}
}

// Nesting level of the agent the context belongs to, zero at the root
func depthOf(ctx context.Context) int {
	return LineageFrom(ctx).Depth
}

// Every child spawned so far, oldest first
func (t *Subagents) Children() []Child {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Child, 0, len(t.order))
	for _, name := range t.order {
		if c := t.children[name]; c != nil {
			out = append(out, *c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// Child by name
func (t *Subagents) Child(name string) (Child, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c, ok := t.children[name]
	if !ok {
		return Child{}, false
	}
	return *c, true
}

// Queues input for a running child
func (t *Subagents) Steer(name string, parts ...protocol.Part) error {
	t.mu.Lock()
	c, ok := t.children[name]
	t.mu.Unlock()
	if !ok {
		return fmt.Errorf("subagent: no child %q", name)
	}
	if !c.Running {
		return fmt.Errorf("subagent: child %q has finished", name)
	}
	c.Agent.Steer(parts...)
	return nil
}

// Stops a running child
func (t *Subagents) Cancel(name string) error {
	t.mu.Lock()
	c, ok := t.children[name]
	t.mu.Unlock()
	if !ok {
		return fmt.Errorf("subagent: no child %q", name)
	}
	if !c.Running {
		return fmt.Errorf("subagent: child %q has finished", name)
	}
	c.cancel()
	return nil
}

// Registers a child under a name no other child holds
func (t *Subagents) register(c *Child) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.children == nil {
		t.children = map[string]*Child{}
		t.used = map[string]bool{}
	}
	t.spawned++
	if c.Name == "" {
		c.Name = fmt.Sprintf("%s-%d", SubagentTool, t.spawned)
	}
	base := c.Name
	for n := 2; t.used[c.Name]; n++ {
		c.Name = fmt.Sprintf("%s-%d", base, n)
	}
	t.used[c.Name] = true
	c.Agent.Name = c.Name
	t.children[c.Name] = c
	t.order = append(t.order, c.Name)
	t.prune()
}

// Forgets the oldest finished children beyond the retention count
func (t *Subagents) prune() {
	finished := 0
	for _, name := range t.order {
		if c := t.children[name]; c != nil && !c.Running {
			finished++
		}
	}
	for i := 0; i < len(t.order) && finished > maxChildren; {
		name := t.order[i]
		if c := t.children[name]; c != nil && !c.Running {
			delete(t.children, name)
			t.order = append(t.order[:i], t.order[i+1:]...)
			finished--
			continue
		}
		i++
	}
}

func (t *Subagents) finish(c *Child, answer string, err error) {
	t.mu.Lock()
	c.Running, c.Finished, c.Answer, c.Err = false, time.Now(), answer, err
	t.prune()
	t.mu.Unlock()
}

func (t *Subagents) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
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
	depth := depthOf(ctx) + 1
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
	parent := policy.AgentFrom(ctx)
	callID := ""
	if call, ok := tool.CallFrom(ctx); ok {
		callID = call.ID
	}
	cctx, cancel := context.WithCancel(WithLineage(ctx, parent, depth))
	defer cancel()
	if t.Timeout > 0 {
		var cancelTimeout context.CancelFunc
		cctx, cancelTimeout = context.WithTimeout(cctx, t.Timeout)
		defer cancelTimeout()
	}
	c := &Child{Name: child.Name, Description: in.Description, Parent: parent, Depth: depth, CallID: callID, Agent: child, Started: time.Now(), Running: true, cancel: cancel}
	t.register(c)
	Observe(ctx, Event{Type: EventSubagentStart, Text: in.Description, Call: protocol.ToolCall{ID: callID, Name: child.Name}, Data: t.snapshot(c)})
	resp, err := child.Run(cctx, []protocol.Part{protocol.Text{Text: in.Prompt}}, func(ev Event) { Observe(ctx, ev) })
	text := strings.TrimSpace(resp.Text())
	switch {
	case err != nil && errors.Is(cctx.Err(), context.DeadlineExceeded) && t.Timeout > 0:
		err = fmt.Errorf("subagent %s timed out after %s", child.Name, t.Timeout)
	case err != nil:
		err = fmt.Errorf("subagent %s failed: %w", child.Name, err)
	case text == "":
		err = fmt.Errorf("subagent %s finished without a text answer", child.Name)
	}
	t.finish(c, text, err)
	end := Event{Type: EventSubagentEnd, Call: protocol.ToolCall{ID: callID, Name: child.Name}, Err: err, Data: t.snapshot(c)}
	if err == nil {
		r := resp
		end.Response = &r
	}
	Observe(ctx, end)
	if err != nil {
		return tool.Error(err.Error()).With(t.snapshot(c)), nil
	}
	return tool.Text(text).With(t.snapshot(c)), nil
}

func (t *Subagents) snapshot(c *Child) Child {
	t.mu.Lock()
	defer t.mu.Unlock()
	return *c
}

var _ tool.Tool = (*Subagents)(nil)
