package sprint

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
)

// List of strings that also reads one string split on newlines and commas, as models often send it
type Strings []string

func (l *Strings) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*l = nil
		for _, part := range strings.FieldsFunc(one, func(r rune) bool { return r == '\n' || r == ',' }) {
			if part = strings.TrimSpace(part); part != "" {
				*l = append(*l, part)
			}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*l = many
	return nil
}

// Stage a sprint is in
type Stage string

const (
	StageBrief  Stage = "brief"
	StagePlan   Stage = "plan"
	StageBuild  Stage = "build"
	StageReport Stage = "report"
	StageDone   Stage = "done"
	StageFailed Stage = "failed"
)

// Stages in the order a sprint goes through them
var Stages = []Stage{StageBrief, StagePlan, StageBuild, StageReport}

// Where a ticket stands
type TicketStatus string

const (
	TicketTodo    TicketStatus = "todo"
	TicketActive  TicketStatus = "active"
	TicketReview  TicketStatus = "review"
	TicketDone    TicketStatus = "done"
	TicketFailed  TicketStatus = "failed"
	TicketBlocked TicketStatus = "blocked"
)

// Reports whether the ticket needs no more work
func (s TicketStatus) Terminal() bool {
	return s == TicketDone || s == TicketFailed || s == TicketBlocked
}

// What a builder hands off about a ticket
type HandoffStatus string

const (
	HandoffDone     HandoffStatus = "done"
	HandoffBlocked  HandoffStatus = "blocked"
	HandoffQuestion HandoffStatus = "question"
)

var handoffStatuses = []HandoffStatus{HandoffDone, HandoffBlocked, HandoffQuestion}

// What the reviewer decides on a handoff
type Verdict string

const (
	VerdictAccepted Verdict = "accepted"
	VerdictRework   Verdict = "rework"
	VerdictGuidance Verdict = "guidance"
	VerdictFailed   Verdict = "failed"
)

var verdicts = []Verdict{VerdictAccepted, VerdictRework, VerdictGuidance, VerdictFailed}

// What the director settled with the person
type Brief struct {
	Objective   string  `json:"objective"`
	Acceptance  Strings `json:"acceptance,omitempty"`
	Constraints Strings `json:"constraints,omitempty"`
	Untouched   Strings `json:"untouched,omitempty"`
	Notes       string  `json:"notes,omitempty"`
}

// How the architect means to build the brief
type Plan struct {
	Title    string  `json:"title"`
	Summary  string  `json:"summary"`
	Approach string  `json:"approach,omitempty"`
	Risks    Strings `json:"risks,omitempty"`
}

// One piece of the plan a builder takes
type Ticket struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Files       Strings `json:"files,omitempty"`
	DependsOn   Strings `json:"depends_on,omitempty"`
	Size        string  `json:"size,omitempty"`
	Status      TicketStatus
	// Builder working or last at work on it
	Builder string
	// Times the reviewer sent it back
	Round int
	// Last handoff the builder made on it
	Report string
	// Notes of the reviewer, oldest first
	Notes []string
	// Why it ended short of done, empty while it did not
	Err      string
	Started  time.Time
	Finished time.Time
}

// What a builder handed off
type Handoff struct {
	Ticket  string        `json:"ticket"`
	Status  HandoffStatus `json:"status"`
	Summary string        `json:"summary"`
	Files   Strings       `json:"files_changed,omitempty"`
	Tested  string        `json:"tested,omitempty"`
}

// What the reviewer decided
type Decision struct {
	Ticket  string  `json:"ticket"`
	Verdict Verdict `json:"verdict"`
	Notes   string  `json:"notes,omitempty"`
}

// One session of the team
type Member struct {
	Role   Role
	Name   string
	Index  int
	Live   *harness.Live
	Joined time.Time
	// Ticket the member works on, empty between tickets
	Ticket string
	// Words on what the member does right now
	Task string
}

// Id of the session of a member, empty while it has none
func (m Member) SessionID() string {
	if m.Live == nil || m.Live.Session == nil {
		return ""
	}
	return m.Live.Session.ID()
}

// Kind of exchange between members, or between a member and the person
type MessageKind string

const (
	MsgStage    MessageKind = "stage"
	MsgAssign   MessageKind = "assign"
	MsgHandoff  MessageKind = "handoff"
	MsgVerdict  MessageKind = "verdict"
	MsgQuestion MessageKind = "question"
	MsgAnswer   MessageKind = "answer"
	MsgNote     MessageKind = "note"
	MsgFailure  MessageKind = "failure"
)

// Name messages use for the person
const Person = "you"

// One exchange the arena shows as it happens
type Message struct {
	Seq    int
	At     time.Time
	Kind   MessageKind
	From   string
	To     string
	Ticket string
	Text   string
}

// Tool call a stage waits for on one session, with a check the call must pass before it is taken
type handoffWait struct {
	tool  string
	check func(any) error
	ch    chan any
}

// One sprint from the request that started it to its end
type Sprint struct {
	mu       sync.Mutex
	ID       string
	Request  string
	Started  time.Time
	Finished time.Time
	Stage    Stage
	// Stage the sprint was in when it ended, done once it finished its work
	Last    Stage
	Status  string
	Err     error
	Brief   Brief
	Plan    Plan
	Tickets []*Ticket
	Members []*Member
	// Exchanges oldest first
	Messages []Message
	seq      int
	// What the director reported at the end
	Summary string
	// Why the sprint waits for the person to tell it to go on, empty while it runs on its own
	Paused string
	// Member whose session the sprint waits on the person to drive, empty while it waits on none
	Waiting string
	// Tokens every member has spent, by name
	Spent map[string]int
	// Closed and replaced whenever the person resumes the sprint, so every waiting stage wakes
	resume chan struct{}
	// Tool calls the stages wait for, by session id
	waits map[string]*handoffWait
	// Tells the interface the sprint changed
	announce func(msg any)
}

// Copy of a sprint safe to read while the workflow goes on
type Snapshot struct {
	ID       string
	Request  string
	Started  time.Time
	Finished time.Time
	Stage    Stage
	Last     Stage
	Status   string
	Err      error
	Brief    Brief
	Plan     Plan
	Tickets  []Ticket
	Members  []Member
	Messages []Message
	Summary  string
	Paused   string
	Waiting  string
	Spent    map[string]int
}

// Reports whether the sprint still goes
func (s Snapshot) Running() bool { return s.Finished.IsZero() }

// Reports whether the person stopped the sprint before it was done
func (s Snapshot) Stopped() bool {
	return !s.Running() && (errors.Is(s.Err, context.Canceled) || errors.Is(s.Err, agent.ErrInterrupted))
}

// Reports whether the sprint ended before its work was done, so it can pick up where it was
func (s Snapshot) Restartable() bool { return !s.Running() && s.Last != StageDone }

// Time the sprint has taken so far or took
func (s Snapshot) Elapsed() time.Duration {
	if s.Finished.IsZero() {
		return time.Since(s.Started)
	}
	return s.Finished.Sub(s.Started)
}

// Tickets done and the count of them
func (s Snapshot) Progress() (done, total int) {
	for _, t := range s.Tickets {
		if t.Status == TicketDone {
			done++
		}
	}
	return done, len(s.Tickets)
}

// Ticket by id, false for an unknown one
func (s Snapshot) Ticket(id string) (Ticket, bool) {
	for _, t := range s.Tickets {
		if t.ID == id {
			return t, true
		}
	}
	return Ticket{}, false
}

// Member by name, false for an unknown one
func (s Snapshot) Member(name string) (Member, bool) {
	for _, m := range s.Members {
		if m.Name == name {
			return m, true
		}
	}
	return Member{}, false
}

// Member serving the session, false for a session of no member
func (s Snapshot) MemberOfSession(id string) (Member, bool) {
	for _, m := range s.Members {
		if m.SessionID() == id {
			return m, true
		}
	}
	return Member{}, false
}

// Messages since a sequence number, oldest first
func (s Snapshot) MessagesAfter(seq int) []Message {
	i := sort.Search(len(s.Messages), func(i int) bool { return s.Messages[i].Seq > seq })
	return s.Messages[i:]
}

func newSprint(id, request string, started time.Time, announce func(any)) *Sprint {
	if announce == nil {
		announce = func(any) {}
	}
	return &Sprint{ID: id, Request: request, Started: started, Stage: StageBrief, Status: "starting", Spent: map[string]int{}, waits: map[string]*handoffWait{}, resume: make(chan struct{}), announce: announce}
}

// Points the sprint at the interface of the process running it now
func (s *Sprint) setAnnounce(announce func(any)) {
	if announce == nil {
		announce = func(any) {}
	}
	s.mu.Lock()
	s.announce = announce
	s.mu.Unlock()
}

// Copy of the sprint as it stands
func (s *Sprint) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Snapshot{ID: s.ID, Request: s.Request, Started: s.Started, Finished: s.Finished, Stage: s.Stage, Last: s.Last, Status: s.Status, Err: s.Err, Brief: s.Brief, Plan: s.Plan, Summary: s.Summary, Paused: s.Paused, Waiting: s.Waiting}
	out.Tickets = make([]Ticket, 0, len(s.Tickets))
	for _, t := range s.Tickets {
		c := *t
		c.Files = slices.Clone(t.Files)
		c.DependsOn = slices.Clone(t.DependsOn)
		c.Notes = slices.Clone(t.Notes)
		out.Tickets = append(out.Tickets, c)
	}
	out.Members = make([]Member, 0, len(s.Members))
	for _, m := range s.Members {
		out.Members = append(out.Members, *m)
	}
	out.Messages = slices.Clone(s.Messages)
	out.Spent = make(map[string]int, len(s.Spent))
	for k, v := range s.Spent {
		out.Spent[k] = v
	}
	return out
}

// Tells the interface the sprint changed
func (s *Sprint) changed() {
	s.mu.Lock()
	announce := s.announce
	s.mu.Unlock()
	announce(ChangedMsg{ID: s.ID})
}

// Records what the director reported at the end
func (s *Sprint) setSummary(text string) {
	s.mu.Lock()
	s.Summary = text
	s.mu.Unlock()
	s.changed()
}

// Moves the sprint to a stage with the words shown beside it
func (s *Sprint) enter(stage Stage, status string) {
	s.mu.Lock()
	s.Stage, s.Status = stage, status
	s.mu.Unlock()
	s.say(MsgStage, "", "", "", string(stage)+": "+status)
}

// Replaces the words shown beside the sprint
func (s *Sprint) setStatus(status string) {
	s.mu.Lock()
	s.Status = status
	s.mu.Unlock()
	s.changed()
}

// Adds an exchange to the log
func (s *Sprint) say(kind MessageKind, from, to, ticket, text string) Message {
	s.mu.Lock()
	s.seq++
	m := Message{Seq: s.seq, At: time.Now(), Kind: kind, From: from, To: to, Ticket: ticket, Text: text}
	s.Messages = append(s.Messages, m)
	s.mu.Unlock()
	s.changed()
	return m
}

// Records how many tokens a member has spent so far
func (s *Sprint) spend(name string, total int) {
	s.mu.Lock()
	s.Spent[name] = total
	s.mu.Unlock()
}

// Adds a member, replacing an earlier one of the same name
func (s *Sprint) putMember(m *Member) {
	s.mu.Lock()
	for i, have := range s.Members {
		if have.Name == m.Name {
			s.Members[i] = m
			s.mu.Unlock()
			s.changed()
			return
		}
	}
	s.Members = append(s.Members, m)
	s.mu.Unlock()
	s.changed()
}

// Member by name, nil for an unknown one
func (s *Sprint) member(name string) *Member {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.Members {
		if m.Name == name {
			return m
		}
	}
	return nil
}

// Member serving the session, nil for a session of no member
func (s *Sprint) memberOfSession(id string) *Member {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.Members {
		if m.SessionID() == id {
			return m
		}
	}
	return nil
}

// Ticket a member holds right now
func (s *Sprint) ticketOf(m *Member) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return m.Ticket
}

// Records what a member does right now and the ticket it holds
func (s *Sprint) task(m *Member, ticket, task string) {
	s.mu.Lock()
	m.Ticket, m.Task = ticket, task
	s.mu.Unlock()
	s.changed()
}

func (s *Sprint) setBrief(b Brief) {
	s.mu.Lock()
	s.Brief = b
	s.mu.Unlock()
	s.changed()
}

func (s *Sprint) setPlan(p Plan, tickets []*Ticket) {
	s.mu.Lock()
	s.Plan, s.Tickets = p, tickets
	s.mu.Unlock()
	s.changed()
}

// Ticket by id, nil for an unknown one
func (s *Sprint) ticket(id string) *Ticket {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.find(id)
}

func (s *Sprint) find(id string) *Ticket {
	for _, t := range s.Tickets {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// Runs fn on a ticket under the lock and tells the interface
func (s *Sprint) update(t *Ticket, fn func(t *Ticket)) {
	s.mu.Lock()
	fn(t)
	s.mu.Unlock()
	s.changed()
}

// Status of a ticket right now
func (s *Sprint) statusOf(t *Ticket) TicketStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return t.Status
}

// Tickets whose dependencies are all done and which nobody has taken, in plan order
func (s *Sprint) ready() []*Ticket {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Ticket
	for _, t := range s.Tickets {
		if t.Status != TicketTodo {
			continue
		}
		open := false
		for _, d := range t.DependsOn {
			if dep := s.find(d); dep == nil || dep.Status != TicketDone {
				open = true
				break
			}
		}
		if !open {
			out = append(out, t)
		}
	}
	return out
}

// Marks every todo ticket whose dependency ended short of done as blocked, reporting how many
func (s *Sprint) blockDependents() int {
	s.mu.Lock()
	n := 0
	for changed := true; changed; {
		changed = false
		for _, t := range s.Tickets {
			if t.Status != TicketTodo {
				continue
			}
			for _, d := range t.DependsOn {
				if dep := s.find(d); dep != nil && (dep.Status == TicketFailed || dep.Status == TicketBlocked) {
					t.Status, t.Err, t.Finished = TicketBlocked, "blocked by ticket "+d+" which ended "+string(dep.Status), time.Now()
					n++
					changed = true
					break
				}
			}
		}
	}
	s.mu.Unlock()
	if n > 0 {
		s.changed()
	}
	return n
}

// Marks every ticket still to do as blocked when nothing is in hand to free it, reporting how many
func (s *Sprint) strand() int {
	s.mu.Lock()
	n := 0
	for _, t := range s.Tickets {
		if t.Status == TicketTodo {
			t.Status, t.Err, t.Finished = TicketBlocked, "blocked, no ticket in hand could free it", time.Now()
			n++
		}
	}
	s.mu.Unlock()
	if n > 0 {
		s.changed()
	}
	return n
}

// Reports whether every ticket needs no more work
func (s *Sprint) settled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.Tickets {
		if !t.Status.Terminal() {
			return false
		}
	}
	return true
}

// Starts waiting for a tool call from the session, replacing an earlier wait, the check refusing unfit calls
func (s *Sprint) await(sessionID, tool string, check func(any) error) *handoffWait {
	w := &handoffWait{tool: tool, check: check, ch: make(chan any, 1)}
	s.mu.Lock()
	s.waits[sessionID] = w
	s.mu.Unlock()
	return w
}

// Stops waiting on the session while the wait is still the one registered
func (s *Sprint) release(sessionID string, w *handoffWait) {
	if w == nil {
		return
	}
	s.mu.Lock()
	if s.waits[sessionID] == w {
		delete(s.waits, sessionID)
	}
	s.mu.Unlock()
}

// Hands a tool call to the stage waiting for it on the session, failing when none waits for that tool
func (s *Sprint) deliver(sessionID, tool string, v any) error {
	s.mu.Lock()
	w := s.waits[sessionID]
	s.mu.Unlock()
	if w == nil {
		return errors.New("no stage of " + s.ID + " waits for " + tool + " from this session right now")
	}
	if w.tool != tool {
		return errors.New("this stage of " + s.ID + " waits for " + w.tool + ", not " + tool)
	}
	if w.check != nil {
		if err := w.check(v); err != nil {
			return err
		}
	}
	select {
	case w.ch <- v:
		return nil
	default:
		return errors.New(tool + " was already called for this stage, end your turn")
	}
}

// Records why the sprint waits for the person to tell it to go on
func (s *Sprint) pause(why string) {
	s.mu.Lock()
	s.Paused = why
	s.mu.Unlock()
	s.changed()
}

// Names the member the sprint waits on the person to drive, empty once it waits on none
func (s *Sprint) wait(name string) {
	s.mu.Lock()
	s.Waiting = name
	s.mu.Unlock()
	s.changed()
}

// Clears the pause and the member waited on once the sprint goes on
func (s *Sprint) unpause() {
	s.mu.Lock()
	s.Paused, s.Waiting = "", ""
	s.mu.Unlock()
	s.changed()
}

// Channel closed when the person next resumes the sprint
func (s *Sprint) resumed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resume
}

// Tells a paused sprint to go on, every stage waiting on the pause waking, false while it is not paused
func (s *Sprint) Resume() bool {
	s.mu.Lock()
	if !s.Finished.IsZero() || s.Paused == "" {
		s.mu.Unlock()
		return false
	}
	s.Paused, s.Waiting = "", ""
	close(s.resume)
	s.resume = make(chan struct{})
	s.mu.Unlock()
	s.changed()
	return true
}

// Asks a running sprint to pause before its next unit of work, false once it has ended or already waits
func (s *Sprint) Pause() bool {
	s.mu.Lock()
	ok := s.Finished.IsZero() && s.Paused == ""
	if ok {
		s.Paused = "paused by you, press resume to go on"
	}
	s.mu.Unlock()
	if ok {
		s.changed()
	}
	return ok
}

// Reports whether the person asked for a pause
func (s *Sprint) paused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Paused != ""
}

// Ends the sprint, failed when err is set and done otherwise, remembering the stage it was in
func (s *Sprint) finish(err error) {
	s.mu.Lock()
	s.Finished, s.Err, s.Last = time.Now(), err, s.Stage
	if err != nil {
		s.Stage, s.Status = StageFailed, err.Error()
	} else {
		s.Stage, s.Last, s.Status = StageDone, StageDone, "done"
	}
	for _, m := range s.Members {
		m.Ticket, m.Task = "", ""
	}
	s.mu.Unlock()
	s.changed()
}

// Stage an ended sprint picks up at, the first whose work is not in hand
func restartStage(s Snapshot) Stage {
	switch {
	case s.Brief.Objective == "":
		return StageBrief
	case len(s.Tickets) == 0:
		return StagePlan
	case s.Last == StageReport:
		return StageReport
	}
	return StageBuild
}

// Readies an ended sprint to run again from its stage, tickets cut short going back to the queue
func (s *Sprint) restart() Stage {
	from := restartStage(s.Snapshot())
	s.mu.Lock()
	s.Finished, s.Err, s.Stage, s.Last, s.Status = time.Time{}, nil, from, "", "restarting"
	s.Paused, s.Waiting = "", ""
	for _, t := range s.Tickets {
		switch {
		case t.Status == TicketActive || t.Status == TicketReview:
			t.Status, t.Err = TicketTodo, ""
		case t.Status == TicketBlocked, t.Status == TicketFailed && !strings.HasPrefix(t.Err, "failed by the reviewer"):
			t.Status, t.Err, t.Finished = TicketTodo, "", time.Time{}
		}
	}
	s.mu.Unlock()
	s.changed()
	return from
}

// Sent to the interface whenever a sprint changes
type ChangedMsg struct {
	ID string
}

var (
	regMu   sync.Mutex
	sprints []*Sprint
)

func register(s *Sprint) {
	regMu.Lock()
	sprints = append(sprints, s)
	regMu.Unlock()
}

// Sprint by id, nil for one this process never started
func sprintByID(id string) *Sprint {
	regMu.Lock()
	defer regMu.Unlock()
	for _, s := range sprints {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// Newest sprint a session serves as a member and the member, false for a session of no sprint
func sprintOfSession(id string) (*Sprint, *Member, bool) {
	regMu.Lock()
	list := slices.Clone(sprints)
	regMu.Unlock()
	for i := len(list) - 1; i >= 0; i-- {
		if m := list[i].memberOfSession(id); m != nil {
			return list[i], m, true
		}
	}
	return nil, nil, false
}

// Every sprint started in this process, newest first
func Sprints() []Snapshot {
	regMu.Lock()
	list := slices.Clone(sprints)
	regMu.Unlock()
	out := make([]Snapshot, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		out = append(out, list[i].Snapshot())
	}
	return out
}

// Snapshot of the sprint with the id, false for one this process never started
func Lookup(id string) (Snapshot, bool) {
	s := sprintByID(id)
	if s == nil {
		return Snapshot{}, false
	}
	return s.Snapshot(), true
}
