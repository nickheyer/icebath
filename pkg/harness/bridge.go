// Bridge between the agent framework and a Bubble Tea program, turning callbacks into messages and prompts into answers
package harness

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/tool"
	"github.com/nickheyer/icebath/pkg/tool/mcp"
	"github.com/nickheyer/icebath/pkg/tool/shell"
)

// Delivers a message into the program from any goroutine
type Sender func(msg any)

// Agent event of one run, text deltas merged per frame
type EventMsg struct {
	Run   string
	Event agent.Event
}

// Output or progress a running tool reported
type ProgressMsg struct {
	Run      string
	Progress tool.Progress
}

// Approval a person must answer
type ApprovalMsg struct {
	Pending *Pending
}

// Approval that was answered, cancelled or timed out
type ApprovalDoneMsg struct {
	ID      int
	Run     string
	Verdict policy.Verdict
	Err     error
}

// Run that reached its end
type RunDoneMsg struct {
	Run      string
	Response protocol.Response
	Err      error
	Duration time.Duration
}

// Background shell job change
type JobMsg struct {
	Event shell.JobEvent
}

// One provider attempt with the telemetry as it stood afterwards
type AttemptMsg struct {
	Attempt   provider.Attempt
	Telemetry Telemetry
}

// MCP server connection change
type MCPMsg struct {
	Server string
	State  mcp.State
	Err    error
	Tools  []string
}

// Session written to the store
type SavedMsg struct {
	Session string
	Path    string
	// Listing entry of the session as saved, so a card need not read the store again
	Info session.Info
	Err  error
}

// Fact store change of one session
type FactMsg struct {
	Session string
	Change  facts.Change
}

// Mission store change of one session
type MissionMsg struct {
	Session string
	Mission mission.Mission
	Op      mission.Op
}

// Hook output that is not part of a verdict, such as plain text a hook printed
type HookOutputMsg struct {
	Run  string
	Text string
}

type runKey struct{}

// Carries the run id so tools and askers can name the run they serve
func WithRun(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, runKey{}, id)
}

// Run id carried by the context, empty when none was set
func RunFrom(ctx context.Context) string {
	id, _ := ctx.Value(runKey{}).(string)
	return id
}

// Error an ask returns once the approver was closed
var ErrClosed = errors.New("harness: approver closed")

// One approval waiting for a person
type Pending struct {
	ID      int
	Run     string
	Agent   string
	Request policy.Request
	Asked   time.Time
	answer  chan policy.Verdict
}

// Asker that posts each request into the program and waits for the answer the program hands back
type Approver struct {
	send    Sender
	mu      sync.Mutex
	next    int
	pending map[int]*Pending
	closed  bool
}

func NewApprover(send Sender) *Approver {
	return &Approver{send: send, pending: map[int]*Pending{}}
}

// Posts the request and blocks until it is answered or the context ends
func (a *Approver) Ask(ctx context.Context, req policy.Request) (policy.Verdict, error) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return policy.No, ErrClosed
	}
	a.next++
	p := &Pending{ID: a.next, Run: RunFrom(ctx), Agent: req.Agent, Request: req, Asked: time.Now(), answer: make(chan policy.Verdict, 1)}
	a.pending[p.ID] = p
	a.mu.Unlock()
	a.send(ApprovalMsg{Pending: p})
	var v policy.Verdict
	var err error
	select {
	case v = <-p.answer:
	case <-ctx.Done():
		err = ctx.Err()
	}
	a.mu.Lock()
	delete(a.pending, p.ID)
	a.mu.Unlock()
	a.send(ApprovalDoneMsg{ID: p.ID, Run: p.Run, Verdict: v, Err: err})
	if err != nil {
		return policy.No, err
	}
	return v, nil
}

// Delivers the answer to a pending approval and reports whether one was waiting
func (a *Approver) Answer(id int, v policy.Verdict) bool {
	a.mu.Lock()
	p, ok := a.pending[id]
	a.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case p.answer <- v:
		return true
	default:
		return false
	}
}

// Answers every pending approval with no
func (a *Approver) DenyAll() int {
	n := 0
	for _, p := range a.Pending() {
		if a.Answer(p.ID, policy.No) {
			n++
		}
	}
	return n
}

// Approvals waiting, oldest first
func (a *Approver) Pending() []*Pending {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*Pending, 0, len(a.pending))
	for _, p := range a.pending {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (a *Approver) Len() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending)
}

// Refuses every later ask and denies the ones waiting
func (a *Approver) Close() {
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
	a.DenyAll()
}

var _ policy.Asker = (*Approver)(nil)

// Frame budget deltas are held for before they are delivered together
const pumpInterval = 16 * time.Millisecond

type deltaKey struct {
	run   string
	agent string
	round int
	index int
	typ   protocol.EventType
}

// Observer that forwards events as messages, merging streamed deltas so a fast model never floods the program
type Pump struct {
	send    Sender
	mu      sync.Mutex
	pending []EventMsg
	index   map[deltaKey]int
	timer   *time.Timer
}

func NewPump(send Sender) *Pump {
	return &Pump{send: send, index: map[deltaKey]int{}}
}

// Observer for one run, safe to call from any goroutine
func (p *Pump) Observer(run string) func(agent.Event) {
	return func(ev agent.Event) { p.Push(run, ev) }
}

func isDelta(ev agent.Event) bool {
	if ev.Type != agent.EventModel {
		return false
	}
	switch ev.Model.Type {
	case protocol.EventTextDelta, protocol.EventThinkingDelta, protocol.EventToolCallDelta, protocol.EventServerToolCallDelta:
		return true
	}
	return false
}

// Delivers an event, holding deltas until the frame ends and flushing them ahead of anything else
func (p *Pump) Push(run string, ev agent.Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	msg := EventMsg{Run: run, Event: ev}
	if !isDelta(ev) {
		batch := p.take()
		for _, m := range batch {
			p.send(m)
		}
		p.send(msg)
		return
	}
	p.mu.Lock()
	key := deltaKey{run, ev.Agent, ev.Round, ev.Model.Index, ev.Model.Type}
	if i, ok := p.index[key]; ok {
		held := &p.pending[i]
		held.Event.Model.Delta += ev.Model.Delta
		held.Event.Model.Partial = ev.Model.Partial
		held.Event.Model.Response = ev.Model.Response
		held.Event.Seq = ev.Seq
		held.Event.Time = ev.Time
	} else {
		p.index[key] = len(p.pending)
		p.pending = append(p.pending, msg)
	}
	if p.timer == nil {
		p.timer = time.AfterFunc(pumpInterval, p.Flush)
	}
	p.mu.Unlock()
}

// Takes every held delta in arrival order and disarms the timer
func (p *Pump) take() []EventMsg {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	if len(p.pending) == 0 {
		return nil
	}
	out := p.pending
	p.pending = nil
	p.index = map[deltaKey]int{}
	return out
}

// Delivers every held delta now
func (p *Pump) Flush() {
	for _, m := range p.take() {
		p.send(m)
	}
}
