package harness

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/agent/subagent"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/tool"
)

// Delay saves wait for more changes before writing
const DefaultSaveDelay = 400 * time.Millisecond

// One agent run the controller started or discovered as a child of one it started
type Run struct {
	ID      string
	Name    string
	Mission string
	Agent   *agent.Agent
	Session *session.Session
	// Run id of the parent, empty at the root
	Parent string
	Depth  int
	// Tool call that spawned a child run
	CallID      string
	Description string
	Started     time.Time
	mu          sync.Mutex
	finished    time.Time
	err         error
	response    protocol.Response
	lastTool    string
	tools       int
	running     bool
	cancel      context.CancelFunc
	done        chan struct{}
}

// Reports whether the run is still going
func (r *Run) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// Time the run ended, zero while it goes
func (r *Run) Finished() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finished
}

// Error the run ended with
func (r *Run) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Final response of a finished run
func (r *Run) Response() protocol.Response {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.response
}

// Name of the last tool called and how many calls the run made
func (r *Run) LastTool() (string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastTool, r.tools
}

// Seconds the run has taken so far or took
func (r *Run) Elapsed(now time.Time) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished.IsZero() {
		return now.Sub(r.Started)
	}
	return r.finished.Sub(r.Started)
}

func (r *Run) finish(at time.Time, resp protocol.Response, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = false
	r.finished = at
	r.response, r.err = resp, err
}

func (r *Run) called(tool string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastTool = tool
	r.tools++
}

// Writes sessions to a store after a short quiet period so a burst costs one write, skipping empty sessions
type Saver struct {
	Store session.Store
	Delay time.Duration
	// Reports whether a session stays off disk, nothing of it written while it does
	Off    func(*session.Session) bool
	send   Sender
	mu     sync.Mutex
	timers map[*session.Session]*time.Timer
	wg     sync.WaitGroup
}

func NewSaver(store session.Store, send Sender) *Saver {
	return &Saver{Store: store, Delay: DefaultSaveDelay, send: send, timers: map[*session.Session]*time.Timer{}}
}

// Schedules a save of the session once changes go quiet
func (s *Saver) Touch(sess *session.Session) {
	if s == nil || s.Store == nil || sess == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, armed := s.timers[sess]; armed {
		return
	}
	delay := s.Delay
	if delay <= 0 {
		delay = DefaultSaveDelay
	}
	s.wg.Add(1)
	s.timers[sess] = time.AfterFunc(delay, func() {
		defer s.wg.Done()
		s.mu.Lock()
		delete(s.timers, sess)
		s.mu.Unlock()
		s.write(sess)
	})
}

// Saves the session now, cancelling any pending save
func (s *Saver) Flush(sess *session.Session) {
	if s == nil || s.Store == nil || sess == nil {
		return
	}
	s.mu.Lock()
	if t, armed := s.timers[sess]; armed {
		if t.Stop() {
			s.wg.Done()
		}
		delete(s.timers, sess)
	}
	s.mu.Unlock()
	s.write(sess)
}

func (s *Saver) write(sess *session.Session) {
	if sess.Empty() || s.Off != nil && s.Off(sess) {
		return
	}
	err := s.Store.Save(context.Background(), sess)
	msg := SavedMsg{Session: sess.ID(), Info: sess.Info(), Err: err}
	if p, ok := s.Store.(session.Pather); ok && err == nil {
		msg.Path, _ = p.Path(sess.ID())
	}
	if s.send != nil {
		s.send(msg)
	}
}

// Waits for every scheduled save to finish
func (s *Saver) Wait() {
	if s == nil {
		return
	}
	s.wg.Wait()
}

// Starts, steers, interrupts and cancels runs and keeps a registry of them and their children
type Controller struct {
	Pump      *Pump
	Saver     *Saver
	Meter     *Meter
	Workspace *Workspace
	Missions  *mission.Store
	// Subagent tool whose children the controller can cancel and steer
	Subagents *subagent.Tool
	// Mission store of a session, the shared one when nil or when it answers nil
	MissionsOf func(*session.Session) *mission.Store
	send       Sender
	mu         sync.Mutex
	runs       map[string]*Run
	order      []string
	active     map[*agent.Agent]bool
	next       int
	Clock      func() time.Time
	wg         sync.WaitGroup
}

func NewController(send Sender, pump *Pump) *Controller {
	return &Controller{Pump: pump, send: send, runs: map[string]*Run{}, active: map[*agent.Agent]bool{}, Clock: time.Now}
}

// Starts a run of the agent with the input, assigning it to a mission when one is named
func (c *Controller) Start(ctx context.Context, a *agent.Agent, missionID string, input []protocol.Part) (*Run, error) {
	if a == nil {
		return nil, errors.New("harness: agent is required")
	}
	c.mu.Lock()
	if c.active[a] || a.Running() {
		c.mu.Unlock()
		return nil, fmt.Errorf("harness: agent %s is already running", a.Name)
	}
	c.active[a] = true
	c.next++
	id := strconv.Itoa(c.next)
	if a.Name == "" {
		a.Name = a.ModelID
	}
	if a.Name == "" {
		a.Name = "root"
	}
	rctx, cancel := context.WithCancel(ctx)
	r := &Run{ID: id, Name: a.Name, Mission: missionID, Agent: a, Session: a.Session, Started: c.Clock(), running: true, cancel: cancel, done: make(chan struct{})}
	c.runs[id] = r
	c.order = append(c.order, id)
	c.mu.Unlock()
	if store := c.missionsOf(a.Session); missionID != "" && store != nil {
		store.Assign(missionID, a.Name)
	}
	rctx = WithRun(rctx, id)
	rctx = mission.WithID(rctx, missionID)
	rctx = tool.WithReporter(rctx, func(p tool.Progress) { c.send(ProgressMsg{Run: id, Progress: p}) })
	observe := c.observer(r)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		resp, err := a.Run(rctx, input, observe)
		finished := c.Clock()
		r.finish(finished, resp, err)
		c.mu.Lock()
		delete(c.active, a)
		c.mu.Unlock()
		cancel()
		if c.Saver != nil {
			c.Saver.Flush(r.Session)
		}
		close(r.done)
		c.send(RunDoneMsg{Run: id, Response: resp, Err: err, Duration: finished.Sub(r.Started)})
	}()
	return r, nil
}

// Observer that tracks children, schedules saves and feeds the meter and workspace before the pump
func (c *Controller) observer(r *Run) func(agent.Event) {
	forward := c.Pump.Observer(r.ID)
	return func(ev agent.Event) {
		switch ev.Type {
		case agent.EventSubagentStart:
			c.childStarted(r, ev)
		case agent.EventSubagentEnd:
			c.childEnded(r, ev)
		case agent.EventToolStart:
			c.mu.Lock()
			owner := c.owner(r, ev.Agent)
			c.mu.Unlock()
			if owner != nil {
				owner.called(ev.Call.Name)
			}
		}
		switch ev.Type {
		case agent.EventTurnEnd, agent.EventToolEnd, agent.EventSteer, agent.EventCompactEnd, agent.EventApproval:
			if c.Saver != nil {
				c.mu.Lock()
				owner := c.owner(r, ev.Agent)
				c.mu.Unlock()
				if owner != nil {
					c.Saver.Touch(owner.Session)
				}
			}
		}
		if c.Meter != nil {
			c.Meter.Apply(r.ID, ev)
		}
		if c.Workspace != nil {
			c.Workspace.Apply(ev)
		}
		forward(ev)
	}
}

// Run the event belongs to, the root when the agent name is its own and the child run otherwise
func (c *Controller) owner(root *Run, agentName string) *Run {
	if agentName == "" || agentName == root.Name {
		return root
	}
	if child, ok := c.runs[root.ID+"/"+agentName]; ok {
		return child
	}
	return root
}

func (c *Controller) childStarted(parent *Run, ev agent.Event) {
	child, ok := ev.Data.(subagent.Child)
	if !ok || child.Agent == nil {
		return
	}
	id := parent.ID + "/" + child.Name
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.runs[id]; dup {
		return
	}
	r := &Run{ID: id, Name: child.Name, Mission: parent.Mission, Agent: child.Agent, Session: child.Agent.Session, Parent: parent.ID, Depth: child.Depth, CallID: child.CallID, Description: child.Description, Started: child.Started, running: true, done: make(chan struct{})}
	if r.Started.IsZero() {
		r.Started = c.Clock()
	}
	c.runs[id] = r
	c.order = append(c.order, id)
}

func (c *Controller) childEnded(parent *Run, ev agent.Event) {
	child, ok := ev.Data.(subagent.Child)
	if !ok {
		return
	}
	c.mu.Lock()
	r, found := c.runs[parent.ID+"/"+child.Name]
	c.mu.Unlock()
	if !found || !r.Running() {
		return
	}
	at := child.Finished
	if at.IsZero() {
		at = c.Clock()
	}
	var resp protocol.Response
	if ev.Response != nil {
		resp = *ev.Response
	}
	r.finish(at, resp, ev.Err)
	close(r.done)
	if c.Saver != nil {
		c.Saver.Flush(r.Session)
	}
}

func (c *Controller) Get(id string) (*Run, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[id]
	return r, ok
}

// Every run in start order
func (c *Controller) Runs() []*Run {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*Run, 0, len(c.order))
	for _, id := range c.order {
		out = append(out, c.runs[id])
	}
	return out
}

// Runs still going, root runs first then children by start time
func (c *Controller) Active() []*Run {
	var out []*Run
	for _, r := range c.Runs() {
		if r.Running() {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Depth != out[j].Depth {
			return out[i].Depth < out[j].Depth
		}
		return out[i].Started.Before(out[j].Started)
	})
	return out
}

// Children of a run in start order
func (c *Controller) Children(id string) []*Run {
	var out []*Run
	for _, r := range c.Runs() {
		if r.Parent == id {
			out = append(out, r)
		}
	}
	return out
}

// Channel closed when the run ends
func (c *Controller) Done(id string) (<-chan struct{}, bool) {
	r, ok := c.Get(id)
	if !ok {
		return nil, false
	}
	return r.done, true
}

// Queues input the agent sees at its next turn
func (c *Controller) Steer(id string, parts ...protocol.Part) error {
	r, ok := c.Get(id)
	if !ok {
		return fmt.Errorf("harness: no agent %s", id)
	}
	if !r.Running() {
		return fmt.Errorf("harness: agent %s has finished", id)
	}
	if r.Parent != "" && c.Subagents != nil {
		return c.Subagents.Steer(r.Name, parts...)
	}
	r.Agent.Steer(parts...)
	return nil
}

// Asks the agent to stop after the current turn or tool round
func (c *Controller) Interrupt(id string) error {
	r, ok := c.Get(id)
	if !ok {
		return fmt.Errorf("harness: no agent %s", id)
	}
	if !r.Running() {
		return fmt.Errorf("harness: agent %s has finished", id)
	}
	r.Agent.Interrupt()
	return nil
}

// Stops the agent at once
func (c *Controller) Cancel(id string) error {
	r, ok := c.Get(id)
	if !ok {
		return fmt.Errorf("harness: no agent %s", id)
	}
	if !r.Running() {
		return fmt.Errorf("harness: agent %s has finished", id)
	}
	if r.Parent != "" {
		if c.Subagents == nil {
			return fmt.Errorf("harness: child agent %s cannot be cancelled without the subagent tool", id)
		}
		return c.Subagents.Cancel(r.Name)
	}
	r.cancel()
	return nil
}

// Stops every run and waits for them to end
func (c *Controller) Shutdown() {
	for _, r := range c.Runs() {
		if r.Running() && r.Parent == "" {
			r.cancel()
		}
	}
	c.wg.Wait()
	if c.Saver != nil {
		c.Saver.Wait()
	}
}

// Reports whether any root run is going
func (c *Controller) Busy() bool {
	for _, r := range c.Runs() {
		if r.Running() && r.Parent == "" {
			return true
		}
	}
	return false
}

// Mission store runs on the session plan into
func (c *Controller) missionsOf(sess *session.Session) *mission.Store {
	if c.MissionsOf != nil {
		if store := c.MissionsOf(sess); store != nil {
			return store
		}
	}
	return c.Missions
}
