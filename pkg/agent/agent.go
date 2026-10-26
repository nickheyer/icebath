// Turn loop that streams the model, gates and runs its tool calls and repeats until it stops
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"sync"
	"sync/atomic"
	"time"

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
	EventRunStart      EventType = "run_start"
	EventRunEnd        EventType = "run_end"
	EventTurnStart     EventType = "turn_start"
	EventTurnEnd       EventType = "turn_end"
	EventModel         EventType = "model"
	EventToolStart     EventType = "tool_start"
	EventToolEnd       EventType = "tool_end"
	EventApproval      EventType = "approval"
	EventSteer         EventType = "steer"
	EventNote          EventType = "note"
	EventCompactStart  EventType = "compact_start"
	EventCompactEnd    EventType = "compact_end"
	EventHook          EventType = "hook"
	EventSubagentStart EventType = "subagent_start"
	EventSubagentEnd   EventType = "subagent_end"
	EventInterrupt     EventType = "interrupt"
)

// Approval asked of the gate, pending until the answer or error is filled in
type Approval struct {
	Request policy.Request
	Answer  policy.Answer
	Err     error
	Pending bool
}

// Progress of a run, stamped with its order, time and the agent lineage that produced it
type Event struct {
	// Position across every agent in the process
	Seq  int64
	Time time.Time
	Type EventType
	// Name of the agent that produced the event
	Agent string
	// Name of the agent that spawned the producer, empty at the root
	Parent string
	Depth  int
	// Model turn the event belongs to, counted from one within a run
	Round    int
	Model    protocol.Event
	Call     protocol.ToolCall
	Result   protocol.ToolResult
	Data     any
	Duration time.Duration
	// Why the call was refused before it ran, or why the run ended
	Err      error
	Approval *Approval
	Parts    []protocol.Part
	Text     string
	// Completed turn on turn end and the final response on run end
	Response *protocol.Response
	// Turn usage on turn end and run total on run end
	Usage protocol.Usage
}

// Error a run ends with after Interrupt was called
var ErrInterrupted = errors.New("agent: interrupted")

// What a hook asks for a tool call before it runs
type Advice struct {
	Call     protocol.ToolCall
	Decision policy.Decision
	// Text appended to the tool result for the model
	Context string
}

// Steps of a run that outside code may observe and rewrite
type Hook interface {
	// Runs before the first turn and may rewrite the user input
	BeforeRun(ctx context.Context, input []protocol.Part) ([]protocol.Part, error)
	// Runs before every model turn and may rewrite the request
	BeforeTurn(ctx context.Context, req *protocol.Request) error
	// Runs after the assistant turn was appended to the session
	AfterTurn(ctx context.Context, resp protocol.Response) error
	// Runs before a tool call and may rewrite it, settle its permission, add context or refuse it with an error
	BeforeTool(ctx context.Context, call protocol.ToolCall) (Advice, error)
	// Runs after a tool call and may rewrite its result
	AfterTool(ctx context.Context, call protocol.ToolCall, result protocol.ToolResult) (protocol.ToolResult, error)
	// Runs when the model stops and may hand back parts that continue the run, again reporting an earlier continuation
	OnStop(ctx context.Context, resp protocol.Response, again bool) ([]protocol.Part, error)
}

// Hook assembled from optional functions, unset ones passing through
type Hooks struct {
	BeforeRunFn  func(ctx context.Context, input []protocol.Part) ([]protocol.Part, error)
	BeforeTurnFn func(ctx context.Context, req *protocol.Request) error
	AfterTurnFn  func(ctx context.Context, resp protocol.Response) error
	BeforeToolFn func(ctx context.Context, call protocol.ToolCall) (Advice, error)
	AfterToolFn  func(ctx context.Context, call protocol.ToolCall, result protocol.ToolResult) (protocol.ToolResult, error)
	OnStopFn     func(ctx context.Context, resp protocol.Response, again bool) ([]protocol.Part, error)
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

func (h Hooks) AfterTurn(ctx context.Context, resp protocol.Response) error {
	if h.AfterTurnFn == nil {
		return nil
	}
	return h.AfterTurnFn(ctx, resp)
}

func (h Hooks) BeforeTool(ctx context.Context, call protocol.ToolCall) (Advice, error) {
	if h.BeforeToolFn == nil {
		return Advice{Call: call}, nil
	}
	return h.BeforeToolFn(ctx, call)
}

func (h Hooks) AfterTool(ctx context.Context, call protocol.ToolCall, result protocol.ToolResult) (protocol.ToolResult, error) {
	if h.AfterToolFn == nil {
		return result, nil
	}
	return h.AfterToolFn(ctx, call, result)
}

func (h Hooks) OnStop(ctx context.Context, resp protocol.Response, again bool) ([]protocol.Part, error) {
	if h.OnStopFn == nil {
		return nil, nil
	}
	return h.OnStopFn(ctx, resp, again)
}

type observerKey struct{}
type lineageKey struct{}

// Who spawned an agent and how deep it sits
type Lineage struct {
	Parent string
	Depth  int
}

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

// Carries the parent and depth a child agent runs under
func WithLineage(ctx context.Context, parent string, depth int) context.Context {
	return context.WithValue(ctx, lineageKey{}, Lineage{Parent: parent, Depth: depth})
}

// Lineage carried by the context, the root lineage when none was set
func LineageFrom(ctx context.Context) Lineage {
	l, _ := ctx.Value(lineageKey{}).(Lineage)
	return l
}

var seq atomic.Int64

// Next event sequence number shared by every agent in the process
func NextSeq() int64 { return seq.Add(1) }

// Model, tools, policy and transcript wired into one loop
type Agent struct {
	Name    string
	Model   Model
	ModelID string
	System  string
	Tools   *tool.Registry
	Gate    policy.Gate
	Session *session.Session
	// Request options copied onto every turn, the output budget left to the model when zero
	MaxTokens   int
	Temperature *float64
	Extra       map[string]json.RawMessage
	// Tool rounds one Run may take before the model must answer, unlimited when zero
	MaxRounds   int
	Hooks       []Hook
	mu          sync.Mutex
	inbox       []protocol.Part
	running     bool
	interrupted bool
}

// Queues input the model sees at the next turn boundary, or as a new turn once it has stopped
func (a *Agent) Steer(parts ...protocol.Part) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.inbox = append(a.inbox, parts...)
}

// Asks the run to stop after the current turn or tool round finishes
func (a *Agent) Interrupt() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.interrupted = true
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
	a.interrupted = false
	return nil
}

func (a *Agent) stop() {
	a.mu.Lock()
	a.running = false
	a.mu.Unlock()
}

func (a *Agent) stopRequested() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.interrupted
}

// Takes everything queued so far
func (a *Agent) drain() []protocol.Part {
	a.mu.Lock()
	defer a.mu.Unlock()
	parts := a.inbox
	a.inbox = nil
	return parts
}

// Observer that stamps every event with order, time and lineage and never runs concurrently
type stamper struct {
	mu      sync.Mutex
	agent   string
	lineage Lineage
	round   *int
	parent  func(Event)
}

func (s *stamper) observe(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev.Agent == "" {
		ev.Agent = s.agent
		ev.Parent = s.lineage.Parent
		ev.Depth = s.lineage.Depth
		ev.Round = *s.round
	}
	if ev.Seq == 0 {
		ev.Seq = NextSeq()
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	s.parent(ev)
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
	round := 0
	st := &stamper{agent: a.Name, lineage: LineageFrom(ctx), round: &round, parent: observe}
	observe = st.observe
	ctx = WithObserver(ctx, observe)
	ctx = policy.WithAgent(ctx, a.Name)
	rounds := a.MaxRounds
	if a.Session.Model() == "" {
		a.Session.SetModel(a.ModelID)
	}
	observe(Event{Type: EventRunStart, Parts: input})
	a.Session.Log(a.Name, "run_start", map[string]any{"model": a.ModelID, "input": protocolText(input)})
	resp, err := a.run(ctx, input, observe, rounds, &round)
	end := Event{Type: EventRunEnd, Err: err, Usage: a.Session.Usage()}
	if err == nil || resp.Content != nil {
		r := resp
		end.Response = &r
	}
	observe(end)
	record := map[string]any{"usage": a.Session.Usage()}
	if err != nil {
		record["error"] = err.Error()
	}
	a.Session.Log(a.Name, "run_end", record)
	return resp, err
}

func (a *Agent) run(ctx context.Context, input []protocol.Part, observe func(Event), rounds int, round *int) (protocol.Response, error) {
	var err error
	for _, h := range a.Hooks {
		if input, err = h.BeforeRun(ctx, input); err != nil {
			return protocol.Response{}, err
		}
	}
	appended := false
	if len(input) > 0 {
		a.Session.Append(protocol.Message{Role: protocol.RoleUser, Content: input})
		appended = true
	}
	continued := false
	for *round = 1; ; *round++ {
		if a.stopRequested() {
			return protocol.Response{}, a.interrupt(ctx, observe)
		}
		a.steer(observe)
		observe(Event{Type: EventTurnStart})
		resp, err := a.turn(ctx, observe)
		if err != nil {
			if *round == 1 && appended {
				a.Session.Truncate(a.Session.Len() - 1)
			}
			return protocol.Response{}, err
		}
		if a.MaxTokens > 0 && resp.StopReason == protocol.StopEnd && resp.Usage.Output >= a.MaxTokens {
			resp.StopReason = protocol.StopLength
		}
		a.Session.AddResponse(a.Name, resp)
		for _, h := range a.Hooks {
			if err := h.AfterTurn(ctx, resp); err != nil {
				return resp, err
			}
		}
		turn := resp
		observe(Event{Type: EventTurnEnd, Response: &turn, Usage: resp.Usage})
		if resp.StopReason != protocol.StopToolUse {
			cont, err := a.stopped(ctx, resp, continued)
			if err != nil {
				return resp, err
			}
			if cont == nil {
				return resp, nil
			}
			if rounds > 0 && *round > rounds {
				return resp, fmt.Errorf("agent: %d rounds reached", rounds)
			}
			continued = true
			a.Session.Append(protocol.Message{Role: protocol.RoleUser, Content: cont})
			observe(Event{Type: EventSteer, Parts: cont, Text: "continued by hook"})
			continue
		}
		calls := resp.ToolCalls()
		if len(calls) == 0 {
			return resp, errors.New("agent: stop reason tool_use with no tool calls")
		}
		if rounds > 0 && *round > rounds {
			limit := fmt.Errorf("agent: %d tool rounds reached", rounds)
			results := make([]protocol.ToolResult, len(calls))
			for i, call := range calls {
				observe(Event{Type: EventToolStart, Call: call})
				results[i] = errorResult(call, limit)
				observe(Event{Type: EventToolEnd, Call: call, Result: results[i], Err: limit})
			}
			a.Session.AddResults(a.Name, results)
			return resp, limit
		}
		a.Session.AddResults(a.Name, a.round(ctx, calls, observe))
		if a.stopRequested() {
			return resp, a.interrupt(ctx, observe)
		}
	}
}

// Records the interrupt and returns the error that ends the run
func (a *Agent) interrupt(_ context.Context, observe func(Event)) error {
	observe(Event{Type: EventInterrupt})
	a.Session.Log(a.Name, "interrupt", nil)
	return ErrInterrupted
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
func (a *Agent) stopped(ctx context.Context, resp protocol.Response, again bool) ([]protocol.Part, error) {
	for _, h := range a.Hooks {
		cont, err := h.OnStop(ctx, resp, again)
		if err != nil {
			return nil, err
		}
		if len(cont) > 0 {
			return cont, nil
		}
	}
	if parts := a.drain(); len(parts) > 0 {
		return parts, nil
	}
	return nil, nil
}

// Streams one model turn, forwarding every event and returning the done response
func (a *Agent) turn(ctx context.Context, observe func(Event)) (protocol.Response, error) {
	extra := make(map[string]json.RawMessage, len(a.Extra))
	for k, v := range a.Extra {
		extra[k] = append(json.RawMessage(nil), v...)
	}
	req := protocol.Request{
		Model:       a.ModelID,
		System:      a.System,
		Messages:    a.Session.Snapshot(),
		MaxTokens:   a.MaxTokens,
		Temperature: a.Temperature,
		Extra:       extra,
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
	contexts := make([]string, len(calls))
	var allowed []protocol.ToolCall
	var slots []int
	gate := a.gate(observe)
	for i, call := range calls {
		observe(Event{Type: EventToolStart, Call: call})
		advice, err := a.beforeTool(ctx, call)
		if err == nil {
			err = gate.Resolve(ctx, advice.Call, advice.Decision)
		}
		if err != nil {
			results[i] = errorResult(advice.Call, err)
			refused[i] = err
			observe(Event{Type: EventToolEnd, Call: advice.Call, Result: results[i], Err: err})
			continue
		}
		calls[i] = advice.Call
		contexts[i] = advice.Context
		allowed = append(allowed, advice.Call)
		slots = append(slots, i)
	}
	if len(allowed) > 0 {
		registry := a.Tools
		if registry == nil {
			registry = &tool.Registry{}
		}
		registry.RunEach(ctx, allowed, func(j int, out tool.Outcome) {
			i := slots[j]
			res := a.afterTool(ctx, calls[i], out.ToolResult)
			if contexts[i] != "" {
				res.Content = append(res.Content, protocol.Text{Text: contexts[i]})
			}
			results[i] = res
			observe(Event{Type: EventToolEnd, Call: calls[i], Result: res, Data: out.Data, Duration: out.Duration})
		})
	}
	return results
}

// Gate copy whose approval hooks publish events and session records
func (a *Agent) gate(observe func(Event)) policy.Gate {
	g := a.Gate
	if g.Tools == nil && a.Tools != nil {
		g.Tools = a.Tools
	}
	userAsk, userAnswer := g.OnAsk, g.OnAnswer
	g.OnAsk = func(req policy.Request) {
		if userAsk != nil {
			userAsk(req)
		}
		observe(Event{Type: EventApproval, Call: req.Call, Approval: &Approval{Request: req, Pending: true}})
	}
	g.OnAnswer = func(req policy.Request, ans policy.Answer, err error) {
		if userAnswer != nil {
			userAnswer(req, ans, err)
		}
		observe(Event{Type: EventApproval, Call: req.Call, Approval: &Approval{Request: req, Answer: ans, Err: err}})
		record := map[string]any{"tool": req.Call.Name, "call_id": req.Call.ID, "allowed": ans.Allowed, "by": ans.By, "verdict": ans.Verdict.String(), "key": req.Key, "reason": ans.Reason}
		if err != nil {
			record["error"] = err.Error()
		}
		a.Session.Log(a.Name, "approval", record)
	}
	return g
}

// Lets every hook rewrite the call, keeping the strictest permission and joining the context any of them added
func (a *Agent) beforeTool(ctx context.Context, call protocol.ToolCall) (Advice, error) {
	out := Advice{Call: call}
	for _, h := range a.Hooks {
		adv, err := h.BeforeTool(ctx, out.Call)
		if err != nil {
			return out, err
		}
		if adv.Call.Name != "" {
			out.Call = adv.Call
		}
		out.Decision = policy.Stricter(out.Decision, adv.Decision)
		if adv.Context != "" {
			if out.Context != "" {
				out.Context += "\n"
			}
			out.Context += adv.Context
		}
	}
	return out, nil
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

func protocolText(parts []protocol.Part) string {
	return protocol.Response{Content: parts}.Text()
}
