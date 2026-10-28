// Missions an operator runs, each an objective with a plan the agent keeps current, persisted as session records
package mission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/tool"
)

// Kind of record a session keeps for every mission change
const RecordKind = "mission"

// Kind of record holding the working plan of a session with no mission attached
const SessionPlanKind = "session_plan"

// Working plan a run keeps on its session while no mission is attached
type SessionPlan struct {
	Steps []Step `json:"steps"`
	Note  string `json:"note,omitempty"`
}

const (
	StepPending = "pending"
	StepActive  = "active"
	StepDone    = "done"
	StepSkipped = "skipped"
)

const (
	StatusActive    = "active"
	StatusDone      = "done"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// One item of a plan
type Step struct {
	Text   string `json:"text"`
	Status string `json:"status"`
}

// One objective with the plan, agents and outcome attached to it
type Mission struct {
	ID        string    `json:"id"`
	Objective string    `json:"objective"`
	Plan      []Step    `json:"plan,omitempty"`
	Agents    []string  `json:"agents,omitempty"`
	Status    string    `json:"status"`
	Started   time.Time `json:"started"`
	Finished  time.Time `json:"finished,omitempty"`
	Note      string    `json:"note,omitempty"`
	// Why the mission failed or was cancelled
	Error string `json:"error,omitempty"`
}

// Share of steps done or skipped, zero without a plan
func (m Mission) Progress() float64 {
	if len(m.Plan) == 0 {
		return 0
	}
	n := 0
	for _, s := range m.Plan {
		if s.Status == StepDone || s.Status == StepSkipped {
			n++
		}
	}
	return float64(n) / float64(len(m.Plan))
}

// Step the agent is working on, or the first pending one
func (m Mission) Current() (Step, bool) {
	for _, s := range m.Plan {
		if s.Status == StepActive {
			return s, true
		}
	}
	for _, s := range m.Plan {
		if s.Status == StepPending {
			return s, true
		}
	}
	return Step{}, false
}

// Reports whether the mission has reached an end state
func (m Mission) Ended() bool {
	return m.Status != StatusActive
}

// What happened to a mission
type Op string

const (
	OpAdd    Op = "add"
	OpUpdate Op = "update"
	OpRemove Op = "remove"
)

// One mission change as listeners and the session log see it, flat so records without an op still read as updates
type Change struct {
	Mission
	Op Op `json:"op,omitempty"`
}

// Returned by a modify callback when nothing changed, so no record is written and no listener runs
var errUnchanged = errors.New("mission: unchanged")

// Missions keyed by id with change listeners and session persistence
type Store struct {
	mu       sync.RWMutex
	missions map[string]*Mission
	order    []string
	next     int
	session  *session.Session
	onChange []func(Change)
}

func New() *Store {
	return &Store{missions: map[string]*Mission{}}
}

// Writes every later change into the session log
func (s *Store) Attach(sess *session.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.session = sess
}

// Registers a listener called after every change outside the store lock
func (s *Store) OnChange(fn func(Change)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onChange = append(s.onChange, fn)
}

// Rebuilds the store from the mission records of a session, replaying adds, updates and removes in order
func (s *Store) Restore(sess *session.Session) error {
	for _, r := range sess.Records() {
		if r.Kind != RecordKind {
			continue
		}
		var c Change
		if err := json.Unmarshal(r.Data, &c); err != nil {
			return fmt.Errorf("mission: record %d: %w", r.Seq, err)
		}
		if c.ID == "" {
			return fmt.Errorf("mission: record %d has no id", r.Seq)
		}
		s.mu.Lock()
		if c.Op == OpRemove {
			s.drop(c.ID)
			s.mu.Unlock()
			continue
		}
		if _, ok := s.missions[c.ID]; !ok {
			s.order = append(s.order, c.ID)
		}
		stored := c.Mission
		s.missions[c.ID] = &stored
		var n int
		if _, err := fmt.Sscanf(c.ID, "m%d", &n); err == nil && n > s.next {
			s.next = n
		}
		s.mu.Unlock()
	}
	return nil
}

// Takes the mission out of the map and the order under the lock
func (s *Store) drop(id string) {
	delete(s.missions, id)
	for i, o := range s.order {
		if o == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

func (s *Store) emit(c Change) {
	s.mu.RLock()
	fns := append([]func(Change){}, s.onChange...)
	sess := s.session
	s.mu.RUnlock()
	if sess != nil {
		sess.Log("", RecordKind, c)
	}
	for _, fn := range fns {
		fn(c)
	}
}

// Starts a mission for an objective
func (s *Store) Create(objective string) (Mission, error) {
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return Mission{}, errors.New("mission: objective is required")
	}
	s.mu.Lock()
	s.next++
	m := Mission{ID: fmt.Sprintf("m%d", s.next), Objective: objective, Status: StatusActive, Started: time.Now().UTC()}
	stored := m
	s.missions[m.ID] = &stored
	s.order = append(s.order, m.ID)
	s.mu.Unlock()
	s.emit(Change{Mission: m, Op: OpAdd})
	return m, nil
}

func (s *Store) Get(id string) (Mission, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.missions[id]
	if !ok {
		return Mission{}, false
	}
	return clone(*m), true
}

// Missions in creation order
func (s *Store) List() []Mission {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Mission, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, clone(*s.missions[id]))
	}
	return out
}

func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.order)
}

// Applies fn to the mission under the lock and publishes the result unless fn reports nothing changed
func (s *Store) modify(id string, fn func(m *Mission) error) (Mission, error) {
	s.mu.Lock()
	m, ok := s.missions[id]
	if !ok {
		s.mu.Unlock()
		return Mission{}, fmt.Errorf("mission: %s does not exist", id)
	}
	err := fn(m)
	out := clone(*m)
	s.mu.Unlock()
	switch {
	case errors.Is(err, errUnchanged):
		return out, nil
	case err != nil:
		return Mission{}, err
	}
	s.emit(Change{Mission: out, Op: OpUpdate})
	return out, nil
}

// Replaces the plan, every step pending unless given a status
func (s *Store) SetPlan(id string, steps []Step) (Mission, error) {
	if err := NormalizeSteps(steps); err != nil {
		return Mission{}, err
	}
	return s.modify(id, func(m *Mission) error {
		m.Plan = append([]Step(nil), steps...)
		return nil
	})
}

// Sets the status of one step by its one based position, demoting any other active step
func (s *Store) SetStep(id string, n int, status string) (Mission, error) {
	switch status {
	case StepPending, StepActive, StepDone, StepSkipped:
	default:
		return Mission{}, fmt.Errorf("mission: unknown step status %q", status)
	}
	return s.modify(id, func(m *Mission) error {
		if n < 1 || n > len(m.Plan) {
			return fmt.Errorf("mission: %s has no step %d", id, n)
		}
		if status == StepActive {
			for i := range m.Plan {
				if m.Plan[i].Status == StepActive {
					m.Plan[i].Status = StepPending
				}
			}
		}
		m.Plan[n-1].Status = status
		return nil
	})
}

// Records that an agent works on the mission, writing nothing when it already is listed
func (s *Store) Assign(id, agent string) (Mission, error) {
	return s.modify(id, func(m *Mission) error {
		for _, a := range m.Agents {
			if a == agent {
				return errUnchanged
			}
		}
		m.Agents = append(m.Agents, agent)
		return nil
	})
}

// Sets the free text note shown with the mission
func (s *Store) SetNote(id, note string) (Mission, error) {
	return s.modify(id, func(m *Mission) error {
		m.Note = strings.TrimSpace(note)
		return nil
	})
}

// Ends the mission with an outcome, keeping the time it finished
func (s *Store) Finish(id, status, reason string) (Mission, error) {
	switch status {
	case StatusDone, StatusFailed, StatusCancelled:
	default:
		return Mission{}, fmt.Errorf("mission: %q is not an end state", status)
	}
	return s.modify(id, func(m *Mission) error {
		m.Status = status
		m.Finished = time.Now().UTC()
		m.Error = strings.TrimSpace(reason)
		return nil
	})
}

// Reopens a finished mission
func (s *Store) Resume(id string) (Mission, error) {
	return s.modify(id, func(m *Mission) error {
		m.Status = StatusActive
		m.Finished = time.Time{}
		m.Error = ""
		return nil
	})
}

// Forgets a mission, logging the removal so it stays gone after a restore
func (s *Store) Remove(id string) bool {
	s.mu.Lock()
	m, ok := s.missions[id]
	if !ok {
		s.mu.Unlock()
		return false
	}
	gone := clone(*m)
	s.drop(id)
	s.mu.Unlock()
	s.emit(Change{Mission: gone, Op: OpRemove})
	return true
}

// Renders the mission as a Markdown checklist
func (m Mission) Markdown() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "## %s (%s, %d%%)\n", m.Objective, m.Status, int(m.Progress()*100+0.5))
	for _, s := range m.Plan {
		mark := " "
		switch s.Status {
		case StepDone:
			mark = "x"
		case StepActive:
			mark = ">"
		case StepSkipped:
			mark = "-"
		}
		fmt.Fprintf(&sb, "- [%s] %s\n", mark, s.Text)
	}
	if m.Note != "" {
		sb.WriteString("\n" + m.Note + "\n")
	}
	return sb.String()
}

func clone(m Mission) Mission {
	m.Plan = append([]Step(nil), m.Plan...)
	m.Agents = append([]string(nil), m.Agents...)
	return m
}

type idKey struct{}

// Carries the mission a run serves so the plan tool knows which one to update
func WithID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, idKey{}, id)
}

// Mission id carried by the context, empty when none was set
func IDFrom(ctx context.Context) string {
	id, _ := ctx.Value(idKey{}).(string)
	return id
}

// Name the model calls to keep its plan current
const ToolName = "Plan"

const schema = `{"type":"object","properties":{
"steps":{"type":"array","items":{"type":"object","properties":{
 "text":{"type":"string","description":"What the step achieves"},
 "status":{"type":"string","enum":["pending","active","done","skipped"]}},"required":["text"],"additionalProperties":false},
 "description":"Complete plan in order, replacing the previous one, at most one step active"},
"note":{"type":"string","description":"Short status note shown beside the plan"}},
"required":["steps"],"additionalProperties":false}`

// Trims step text, fills empty statuses with pending and rejects unknown statuses
func NormalizeSteps(steps []Step) error {
	for i := range steps {
		steps[i].Text = strings.TrimSpace(steps[i].Text)
		if steps[i].Text == "" {
			return fmt.Errorf("mission: step %d has no text", i+1)
		}
		switch steps[i].Status {
		case "":
			steps[i].Status = StepPending
		case StepPending, StepActive, StepDone, StepSkipped:
		default:
			return fmt.Errorf("mission: step %d has unknown status %q", i+1, steps[i].Status)
		}
	}
	return nil
}

// Share of steps done or skipped
func stepsProgress(steps []Step) float64 {
	if len(steps) == 0 {
		return 0
	}
	done := 0
	for _, s := range steps {
		if s.Status == StepDone || s.Status == StepSkipped {
			done++
		}
	}
	return float64(done) / float64(len(steps))
}

// Numbered plan with the share done on top
func formatPlan(steps []Step, progress float64) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Plan updated, %d%% done\n", int(progress*100+0.5))
	for i, s := range steps {
		fmt.Fprintf(&sb, "%d. [%s] %s\n", i+1, s.Status, s.Text)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// Working plan last kept on the session, false when none was
func CurrentSessionPlan(sess *session.Session) (SessionPlan, bool) {
	records := sess.Records()
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].Kind != SessionPlanKind {
			continue
		}
		var p SessionPlan
		if err := json.Unmarshal(records[i].Data, &p); err != nil {
			return SessionPlan{}, false
		}
		return p, true
	}
	return SessionPlan{}, false
}

// Tool that replaces the plan of the mission the calling run serves, or of its session when no mission is attached
type Tool struct {
	// Store of the calling run
	Resolve func(context.Context) *Store
	// Session of the calling run, holding the plan while no mission is attached
	Session func(context.Context) *session.Session
}

func (Tool) Definition() protocol.Tool {
	return protocol.Tool{
		Name:        ToolName,
		Description: "Replaces the plan with the full ordered list of steps and their statuses, on the attached mission or on this session when none is attached. Call it before work that takes more than a few tool calls and whenever a step changes state, keeping exactly one step active.",
		Parameters:  json.RawMessage(schema),
	}
}

func (Tool) Annotations() tool.Annotations {
	return tool.Annotations{Idempotent: true}
}

func (t Tool) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	store := t.Resolve(ctx)
	id := IDFrom(ctx)
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
	if id == "" {
		var sess *session.Session
		if t.Session != nil {
			sess = t.Session(ctx)
		}
		if sess == nil {
			return tool.Error("no mission is attached and no session holds a plan"), nil
		}
		if err := NormalizeSteps(in.Steps); err != nil {
			return tool.Error(err.Error()), nil
		}
		plan := SessionPlan{Steps: in.Steps, Note: in.Note}
		if _, err := sess.Log(policy.AgentFrom(ctx), SessionPlanKind, plan); err != nil {
			return tool.Error(err.Error()), nil
		}
		return tool.Text(formatPlan(in.Steps, stepsProgress(in.Steps))).With(plan), nil
	}
	m, err := store.SetPlan(id, in.Steps)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if in.Note != "" {
		if m, err = store.SetNote(id, in.Note); err != nil {
			return tool.Error(err.Error()), nil
		}
	}
	return tool.Text(formatPlan(m.Plan, m.Progress())).With(m), nil
}

var _ tool.Tool = Tool{}
