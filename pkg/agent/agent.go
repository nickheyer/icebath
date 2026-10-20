// Turn loop that streams the model, gates and runs its tool calls and repeats until it stops
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"sync"

	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/tool"
)

// Streams one turn, satisfied by a provider
type Model interface {
	Stream(ctx context.Context, req protocol.Request) iter.Seq2[protocol.Event, error]
}

type EventType string

const (
	EventModel     EventType = "model"
	EventToolStart EventType = "tool_start"
	EventToolEnd   EventType = "tool_end"
	EventSteer     EventType = "steer"
	EventNote      EventType = "note"
)

// Progress of a run, carrying a protocol event, one side of a tool call, steered input or a note
type Event struct {
	Type EventType
	// Name of the agent that produced the event
	Agent  string
	Model  protocol.Event
	Call   protocol.ToolCall
	Result protocol.ToolResult
	// Why the call was refused before it ran, on tool end events
	Err   error
	Parts []protocol.Part
	Text  string
}

// Rounds of tool calls one Run may make before giving up
const DefaultMaxRounds = 50

// Steps of a run that outside code may observe and rewrite
type Hook interface {
	// Runs before the first turn and may rewrite the user input
	BeforeRun(ctx context.Context, input []protocol.Part) ([]protocol.Part, error)
	// Runs before every model turn and may rewrite the request
	BeforeTurn(ctx context.Context, req *protocol.Request) error
	// Runs before a tool call and may rewrite it, settle its permission or refuse it with an error
	BeforeTool(ctx context.Context, call protocol.ToolCall) (protocol.ToolCall, policy.Decision, error)
	// Runs after a tool call and may rewrite its result
	AfterTool(ctx context.Context, call protocol.ToolCall, result protocol.ToolResult) (protocol.ToolResult, error)
	// Runs when the model stops and may hand back parts that continue the run as a user message
	OnStop(ctx context.Context, resp protocol.Response) ([]protocol.Part, error)
}

// Hook assembled from optional functions, unset ones passing through
type Hooks struct {
	BeforeRunFn  func(ctx context.Context, input []protocol.Part) ([]protocol.Part, error)
	BeforeTurnFn func(ctx context.Context, req *protocol.Request) error
	BeforeToolFn func(ctx context.Context, call protocol.ToolCall) (protocol.ToolCall, policy.Decision, error)
	AfterToolFn  func(ctx context.Context, call protocol.ToolCall, result protocol.ToolResult) (protocol.ToolResult, error)
	OnStopFn     func(ctx context.Context, resp protocol.Response) ([]protocol.Part, error)
}

func (h Hooks) BeforeRun(ctx context.Context, input []protocol.Part) ([]protocol.Part, error) {
	if h.BeforeRunFn == nil {
		return input, nil
	}
	return h.BeforeRunFn(ctx, input)
}

func (h Hooks) BeforeTurn(ctx context.Context, req *protocol.Request) error {
	if h.BeforeTurnFn == nil {
		return nil
	}
	return h.BeforeTurnFn(ctx, req)
}

func (h Hooks) BeforeTool(ctx context.Context, call protocol.ToolCall) (protocol.ToolCall, policy.Decision, error) {
	if h.BeforeToolFn == nil {
		return call, "", nil
	}
	return h.BeforeToolFn(ctx, call)
}

func (h Hooks) AfterTool(ctx context.Context, call protocol.ToolCall, result protocol.ToolResult) (protocol.ToolResult, error) {
	if h.AfterToolFn == nil {
		return result, nil
	}
	return h.AfterToolFn(ctx, call, result)
}

func (h Hooks) OnStop(ctx context.Context, resp protocol.Response) ([]protocol.Part, error) {
	if h.OnStopFn == nil {
		return nil, nil
	}
	return h.OnStopFn(ctx, resp)
}

type observerKey struct{}

// Carries an observer so tools and hooks can report events into the run that called them
func WithObserver(ctx context.Context, observe func(Event)) context.Context {
	return context.WithValue(ctx, observerKey{}, observe)
}

// Reports an event to the observer of the run the context belongs to, if any
func Observe(ctx context.Context, ev Event) {
	if observe, ok := ctx.Value(observerKey{}).(func(Event)); ok {
		observe(ev)
	}
}

// Model, tools, policy and transcript wired into one loop
type Agent struct {
	Name    string
	Model   Model
	ModelID string
	System  string
	Tools   *tool.Registry
	Gate    policy.Gate
	Session *session.Session
	// Request knobs copied onto every turn
	MaxTokens   int
	Temperature *float64
	Extra       map[string]json.RawMessage
	MaxRounds   int
	Hooks       []Hook
	mu          sync.Mutex
	inbox       []protocol.Part
	running     bool
}

// Queues input the model sees at the next turn boundary, or as a new turn once it has stopped
func (a *Agent) Steer(parts ...protocol.Part) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.inbox = append(a.inbox, parts...)
}

// Reports whether a run is in progress
func (a *Agent) Running() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

func (a *Agent) start() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return errors.New("agent: already running")
	}
	a.running = true
	return nil
}

func (a *Agent) stop() {
	a.mu.Lock()
	a.running = false
	a.mu.Unlock()
}

// Takes everything queued so far
func (a *Agent) drain() []protocol.Part {
	a.mu.Lock()
	defer a.mu.Unlock()
	parts := a.inbox
	a.inbox = nil
	return parts
}

// Appends the user input, then loops model turns and tool rounds until the model stops
func (a *Agent) Run(ctx context.Context, input []protocol.Part, observe func(Event)) (protocol.Response, error) {
	if a.Model == nil || a.Session == nil || a.ModelID == "" {
		return protocol.Response{}, errors.New("agent: model, model id and session are required")
	}
	if err := a.start(); err != nil {
		return protocol.Response{}, err
	}
	defer a.stop()
	if observe == nil {
		observe = func(Event) {}
	}
	parent := observe
	observe = func(ev Event) {
		if ev.Agent == "" {
			ev.Agent = a.Name
		}
		parent(ev)
	}
	ctx = WithObserver(ctx, observe)
	rounds := a.MaxRounds
	if rounds <= 0 {
		rounds = DefaultMaxRounds
	}
	var err error
	for _, h := range a.Hooks {
		if input, err = h.BeforeRun(ctx, input); err != nil {
			return protocol.Response{}, err
		}
	}
	if len(input) > 0 {
		a.Session.Append(protocol.Message{Role: protocol.RoleUser, Content: input})
	}
	for round := 0; ; round++ {
		a.steer(observe)
		resp, err := a.turn(ctx, observe)
		if err != nil {
			return protocol.Response{}, err
		}
		a.Session.AddResponse(resp)
		if resp.StopReason != protocol.StopToolUse {
			cont, err := a.stopped(ctx, resp)
			if err != nil {
				return resp, err
			}
			if cont == nil {
				return resp, nil
			}
			if round+1 >= rounds {
				return resp, fmt.Errorf("agent: %d rounds reached", rounds)
			}
			a.Session.Append(protocol.Message{Role: protocol.RoleUser, Content: cont})
			continue
		}
		calls := toolCalls(resp)
		if len(calls) == 0 {
			return resp, errors.New("agent: stop reason tool_use with no tool calls")
		}
		if round+1 >= rounds {
			return resp, fmt.Errorf("agent: %d tool rounds reached", rounds)
		}
		a.Session.AddResults(a.round(ctx, calls, observe))
	}
}

// Delivers queued input as a user message ahead of the next turn
func (a *Agent) steer(observe func(Event)) {
	parts := a.drain()
	if len(parts) == 0 {
		return
	}
	a.Session.Append(protocol.Message{Role: protocol.RoleUser, Content: parts})
	observe(Event{Type: EventSteer, Parts: parts})
}

// Gives stop hooks and queued input a chance to continue the run
func (a *Agent) stopped(ctx context.Context, resp protocol.Response) ([]protocol.Part, error) {
	for _, h := range a.Hooks {
		cont, err := h.OnStop(ctx, resp)
		if err != nil {
			return nil, err
		}
		if len(cont) > 0 {
			return cont, nil
		}
	}
	if parts := a.drain(); len(parts) > 0 {
		Observe(ctx, Event{Type: EventSteer, Parts: parts})
		return parts, nil
	}
	return nil, nil
}

// Streams one model turn, forwarding every event and returning the done response
func (a *Agent) turn(ctx context.Context, observe func(Event)) (protocol.Response, error) {
	req := protocol.Request{
		Model:       a.ModelID,
		System:      a.System,
		Messages:    a.Session.Snapshot(),
		MaxTokens:   a.MaxTokens,
		Temperature: a.Temperature,
		Extra:       a.Extra,
		Stream:      true,
	}
	if a.Tools != nil {
		req.Tools = a.Tools.Definitions()
	}
	for _, h := range a.Hooks {
		if err := h.BeforeTurn(ctx, &req); err != nil {
			return protocol.Response{}, err
		}
	}
	var done *protocol.Response
	for ev, err := range a.Model.Stream(ctx, req) {
		if err != nil {
			return protocol.Response{}, err
		}
		observe(Event{Type: EventModel, Model: ev})
		if ev.Type == protocol.EventDone {
			resp := ev.Response
			done = &resp
		}
	}
	if done == nil {
		return protocol.Response{}, errors.New("agent: stream ended without a done event")
	}
	return *done, nil
}

// Gates every call, runs the allowed ones together and returns results in call order
func (a *Agent) round(ctx context.Context, calls []protocol.ToolCall, observe func(Event)) []protocol.ToolResult {
	results := make([]protocol.ToolResult, len(calls))
	refused := make([]error, len(calls))
	var allowed []protocol.ToolCall
	var slots []int
	for i, call := range calls {
		observe(Event{Type: EventToolStart, Call: call})
		call, settled, err := a.beforeTool(ctx, call)
		if err == nil {
			err = a.Gate.Resolve(ctx, call, settled)
		}
		if err != nil {
			results[i] = errorResult(call, err)
			refused[i] = err
			continue
		}
		calls[i] = call
		allowed = append(allowed, call)
		slots = append(slots, i)
	}
	if len(allowed) > 0 {
		registry := a.Tools
		if registry == nil {
			registry = &tool.Registry{}
		}
		for j, res := range registry.RunAll(ctx, allowed) {
			results[slots[j]] = res
		}
	}
	for i, res := range results {
		if refused[i] == nil {
			results[i] = a.afterTool(ctx, calls[i], res)
		}
		observe(Event{Type: EventToolEnd, Call: calls[i], Result: results[i], Err: refused[i]})
	}
	return results
}

// Lets every hook rewrite the call, keeping the strictest permission any of them settled
func (a *Agent) beforeTool(ctx context.Context, call protocol.ToolCall) (protocol.ToolCall, policy.Decision, error) {
	var settled policy.Decision
	for _, h := range a.Hooks {
		var d policy.Decision
		var err error
		if call, d, err = h.BeforeTool(ctx, call); err != nil {
			return call, settled, err
		}
		settled = policy.Stricter(settled, d)
	}
	return call, settled, nil
}

func (a *Agent) afterTool(ctx context.Context, call protocol.ToolCall, res protocol.ToolResult) protocol.ToolResult {
	for _, h := range a.Hooks {
		var err error
		if res, err = h.AfterTool(ctx, call, res); err != nil {
			return errorResult(call, err)
		}
	}
	return res
}

func errorResult(call protocol.ToolCall, err error) protocol.ToolResult {
	return protocol.ToolResult{CallID: call.ID, Name: call.Name, Content: []protocol.Part{protocol.Text{Text: err.Error()}}, IsError: true}
}

func toolCalls(resp protocol.Response) []protocol.ToolCall {
	var calls []protocol.ToolCall
	for _, p := range resp.Content {
		if c, ok := p.(protocol.ToolCall); ok {
			calls = append(calls, c)
		}
	}
	return calls
}

// Joins the text parts of a response
func Text(resp protocol.Response) string {
	var out string
	for _, p := range resp.Content {
		if t, ok := p.(protocol.Text); ok {
			out += t.Text
		}
	}
	return out
}
