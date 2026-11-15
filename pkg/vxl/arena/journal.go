package arena

import (
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Tools of one agent, the one it runs now and how many of each name it started
type Tools struct {
	Current string
	Since   time.Time
	Counts  map[string]int
}

// One delegated subagent as the harness reports it
type Agent struct {
	ID           string
	Parent       string
	Goal         string
	Status       string
	Model        string
	Summary      string
	Depth        int
	ToolCount    int
	Tools        Tools
	Thinking     []string
	Notes        []string
	Output       []string
	FilesRead    []string
	FilesWritten []string
	Toolsets     []string
	Usage        Usage
	Started      time.Time
	Duration     time.Duration
}

// One todo item of the main agent
type Todo struct {
	ID      string
	Parent  string
	Content string
	Status  string
}

// State of a session at one moment as the harness reports it, folded into the journal on every change
type Report struct {
	Session    string
	At         time.Time
	Busy       bool
	Prompt     string
	Model      string
	Thinking   string
	Streaming  bool
	Outcome    Outcome
	Failure    string
	Tools      Tools
	Todos      []Todo
	Agents     []Agent
	Approval   string
	Clarify    string
	Paused     bool
	Usage      Usage
	Context    int
	ContextMax int
}

// Tokens and tool calls that light a member fully when no real budget is known
const (
	nominalTokens = 400_000
	nominalTools  = 50
)

// Most turns a session keeps
const keptSprints = 64

// Goals that read as review or verification work
var reviewGoal = regexp.MustCompile(`(?i)\b(review|verif|validat|audit|critiqu|proofread|cross-check|double-check|sanity-check)`)

// Orchestration of every session a terminal showed, each turn kept as a sprint with the tape it played
type Journal struct {
	mu    sync.Mutex
	rev   uint64
	newer string
	books map[string]*book
}

// Turns of one session and which turn owns each agent
type book struct {
	session string
	sprints []*sprint
	owner   map[string]*sprint
	busy    bool
	turns   int
	opened  int
	at      time.Time
}

// One turn of a session, its state now and every change that led to it
type sprint struct {
	id       string
	turn     int
	title    string
	started  time.Time
	turnOpen bool
	base     Usage
	cur      state
	tape     []change
	messages []Message
	byAgent  map[string]int
	ticketOf map[string]string
	crew     int
	tickets  int
	todos    int
}

// Everything a sprint shows at one moment
type state struct {
	members []Member
	tickets []Ticket
	live    map[string]liveState
	usage   map[string]Usage
	stage   Stage
	waiting string
	paused  string
	outcome Outcome
	failure string
	running bool
	ended   time.Time
}

// What one change of a sprint replaced
type changeKind int

const (
	changeMember changeKind = iota
	changeTicket
	changeLive
	changeUsage
	changeStage
	changeWaiting
	changePaused
	changeRun
)

// One change of a sprint at a moment, the new value of what it names
type change struct {
	at      time.Time
	kind    changeKind
	name    string
	member  *Member
	ticket  *Ticket
	live    *liveState
	usage   *Usage
	text    string
	outcome Outcome
	running bool
}

// Journal holding no session yet
func NewJournal() *Journal { return &Journal{books: map[string]*book{}} }

// Count that grows with every change, so a reader knows when to look again
func (j *Journal) Revision() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.rev
}

// Session the newest report came from
func (j *Journal) Session() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.newer
}

// Ids of the turns of a session, oldest first
func (j *Journal) Sprints(session string) []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	b := j.books[session]
	if b == nil {
		return nil
	}
	out := make([]string, len(b.sprints))
	for i, sp := range b.sprints {
		out[i] = sp.id
	}
	return out
}

// Turn with an id and the book holding it
func (j *Journal) find(id string) (*sprint, *book) {
	for _, b := range j.books {
		for _, sp := range b.sprints {
			if sp.id == id {
				return sp, b
			}
		}
	}
	return nil, nil
}

// State of a turn at a moment, now for the zero time, with what each member did then
func (j *Journal) View(id string, at time.Time) (*Snapshot, map[string]liveState, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	sp, b := j.find(id)
	if sp == nil {
		return nil, nil, false
	}
	st := &sp.cur
	when := b.at
	if !at.IsZero() {
		st, when = sp.replay(at), at
	}
	snap := &Snapshot{ID: sp.id, Sprint: sp.id, Turn: sp.turn, Title: sp.title, Stage: st.stage, Started: sp.started, Ended: st.ended, At: when,
		Members: slices.Clone(st.members), Tickets: slices.Clone(st.tickets), Usage: maps.Clone(st.usage), Waiting: st.waiting, Paused: st.paused,
		Outcome: st.outcome, Live: st.running, Newest: b.sprints[len(b.sprints)-1] == sp}
	if st.failure != "" {
		snap.Err = fmt.Errorf("%s", st.failure)
	}
	n := len(sp.messages)
	if !at.IsZero() {
		n, _ = slices.BinarySearchFunc(sp.messages, at, func(m Message, t time.Time) int {
			if m.At.After(t) {
				return 1
			}
			return -1
		})
	}
	snap.Messages = slices.Clone(sp.messages[:n])
	return snap, maps.Clone(st.live), true
}

// Whether a turn still runs and when it ended, false for an unknown turn
func (j *Journal) Status(id string) (running bool, ended time.Time, ok bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	sp, _ := j.find(id)
	if sp == nil {
		return false, time.Time{}, false
	}
	return sp.cur.running, sp.cur.ended, true
}

// Moments a turn changed at, its first and last, for the timeline
type tape struct {
	start, end time.Time
	times      []time.Time
	marks      []Message
}

// Tape of a turn, its end at now while it still runs
func (j *Journal) Tape(id string) (tape, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	sp, b := j.find(id)
	if sp == nil {
		return tape{}, false
	}
	t := tape{start: sp.started, end: b.at, marks: slices.Clone(sp.messages)}
	if !sp.cur.running && !sp.cur.ended.IsZero() {
		t.end = sp.cur.ended
	}
	for _, c := range sp.tape {
		if n := len(t.times); n == 0 || c.at.After(t.times[n-1]) {
			t.times = append(t.times, c.at)
		}
	}
	for _, m := range sp.messages {
		if i, found := slices.BinarySearchFunc(t.times, m.At, func(a, b time.Time) int { return a.Compare(b) }); !found {
			t.times = slices.Insert(t.times, i, m.At)
		}
	}
	return t, true
}

// Records guidance the person sent a member of a turn
func (j *Journal) Guide(id, name, text string, at time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	sp, b := j.find(id)
	if sp == nil {
		return
	}
	at = b.clock(at)
	sp.say(at, MsgVerdict, Person, name, "", string(VerdictGuidance)+": "+text)
	j.rev++
}

// Folds a report into the turns of its session, recording every change and the exchanges it implies
func (j *Journal) Observe(r Report) {
	if r.Session == "" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.newer = r.Session
	b := j.books[r.Session]
	if b == nil {
		b = &book{session: r.Session, owner: map[string]*sprint{}}
		j.books[r.Session] = b
	}
	at := b.clock(r.At)
	before := b.revision()
	if r.Busy && !b.busy {
		b.turns++
		b.open(r, at, true)
	}
	if len(b.sprints) == 0 && slices.ContainsFunc(r.Agents, func(a Agent) bool { return activeStatus(a.Status) }) {
		b.open(r, at, false)
	}
	if len(b.sprints) == 0 {
		b.busy = r.Busy
		return
	}
	newest := b.sprints[len(b.sprints)-1]
	if newest.turnOpen {
		newest.director(r, at)
	}
	for _, ag := range r.Agents {
		if ag.ID == "" {
			continue
		}
		owner := b.owner[ag.ID]
		if owner == nil {
			owner = newest
			if parent := b.owner[ag.Parent]; parent != nil && ag.Parent != "" {
				owner = parent
			}
			b.owner[ag.ID] = owner
		}
		owner.agent(ag, r.Agents, at)
	}
	present := map[string]bool{}
	for _, ag := range r.Agents {
		present[ag.ID] = true
	}
	for _, sp := range b.sprints {
		for _, m := range slices.Clone(sp.cur.members) {
			if m.ID != "" && activeStatus(m.Status) && !present[m.ID] {
				sp.vanish(at, m)
			}
		}
	}
	if b.busy && !r.Busy && newest.turnOpen {
		newest.endTurn(r, at)
	}
	for _, sp := range b.sprints {
		sp.settle(at)
	}
	b.busy = r.Busy
	if b.revision() != before {
		j.rev++
	}
}

// Moment of a report, never before the last one so the tape stays in order
func (b *book) clock(at time.Time) time.Time {
	if at.Before(b.at) {
		at = b.at
	}
	b.at = at
	return at
}

// Sum of the changes and messages of every turn, which grows whenever anything was recorded
func (b *book) revision() int {
	n := len(b.sprints)
	for _, sp := range b.sprints {
		n += len(sp.tape) + len(sp.messages)
	}
	return n
}

// Opens a turn of the main agent, or one for agents found already running
func (b *book) open(r Report, at time.Time, turn bool) {
	b.opened++
	sp := &sprint{id: fmt.Sprintf("%s#%d", b.session, b.opened), turn: b.turns, started: at, turnOpen: turn, base: r.Usage,
		cur: state{live: map[string]liveState{}, usage: map[string]Usage{}}, byAgent: map[string]int{}, ticketOf: map[string]string{}}
	if turn {
		sp.title = strings.TrimSpace(r.Prompt)
		sp.record(change{at: at, kind: changeStage, text: string(StageBrief)})
		sp.record(change{at: at, kind: changeRun, running: true})
	}
	sp.record(change{at: at, kind: changeMember, member: &Member{Name: directorName, Role: RoleDirector, Task: sp.title, Model: r.Model, Status: "idle", Started: at}})
	b.sprints = append(b.sprints, sp)
	if over := len(b.sprints) - keptSprints; over > 0 {
		for id, owner := range b.owner {
			if slices.Contains(b.sprints[:over], owner) {
				delete(b.owner, id)
			}
		}
		b.sprints = slices.Delete(b.sprints, 0, over)
	}
}

// Applies a change to the state now and keeps it on the tape
func (sp *sprint) record(c change) {
	sp.cur.apply(c)
	sp.tape = append(sp.tape, c)
}

// State of the turn as it stood at a moment, the tape played up to it
func (sp *sprint) replay(at time.Time) *state {
	st := &state{live: map[string]liveState{}, usage: map[string]Usage{}}
	for _, c := range sp.tape {
		if c.at.After(at) {
			break
		}
		st.apply(c)
	}
	return st
}

// Takes one change into the state
func (st *state) apply(c change) {
	switch c.kind {
	case changeMember:
		if i := slices.IndexFunc(st.members, func(m Member) bool { return m.Name == c.member.Name }); i >= 0 {
			st.members[i] = *c.member
		} else {
			st.members = append(st.members, *c.member)
		}
	case changeTicket:
		if i := slices.IndexFunc(st.tickets, func(t Ticket) bool { return t.ID == c.ticket.ID }); i >= 0 {
			st.tickets[i] = *c.ticket
		} else {
			st.tickets = append(st.tickets, *c.ticket)
		}
	case changeLive:
		st.live[c.name] = *c.live
	case changeUsage:
		st.usage[c.name] = *c.usage
	case changeStage:
		st.stage = Stage(c.text)
	case changeWaiting:
		st.waiting = c.text
	case changePaused:
		st.paused = c.text
	case changeRun:
		st.running, st.outcome, st.failure = c.running, c.outcome, c.text
		st.ended = time.Time{}
		if !c.running {
			st.ended = c.at
		}
	}
}

// Appends a message to the turn
func (sp *sprint) say(at time.Time, kind MsgKind, from, to, ticket, text string) {
	sp.messages = append(sp.messages, Message{Seq: len(sp.messages) + 1, At: at, Kind: kind, From: from, To: to, Ticket: ticket, Text: text})
}

// Member with a name as it stands now
func (sp *sprint) member(name string) (Member, bool) {
	i := slices.IndexFunc(sp.cur.members, func(m Member) bool { return m.Name == name })
	if i < 0 {
		return Member{}, false
	}
	return sp.cur.members[i], true
}

// Ticket with an id as it stands now
func (sp *sprint) ticket(id string) (Ticket, bool) {
	i := slices.IndexFunc(sp.cur.tickets, func(t Ticket) bool { return t.ID == id })
	if i < 0 {
		return Ticket{}, false
	}
	return sp.cur.tickets[i], true
}

// Records a member when it differs from how it stands
func (sp *sprint) putMember(at time.Time, m Member) {
	if have, ok := sp.member(m.Name); ok && reflect.DeepEqual(have, m) {
		return
	}
	sp.record(change{at: at, kind: changeMember, member: &m})
}

// Records a ticket when it differs from how it stands, counting a round each time it goes back to work
func (sp *sprint) putTicket(at time.Time, t Ticket) {
	have, ok := sp.ticket(t.ID)
	if ok {
		t.Round, t.Opened, t.Closed = have.Round, have.Opened, have.Closed
		if t.Status == TicketActive && have.Status != TicketActive && have.Status != TicketTodo {
			t.Round++
		}
	} else {
		t.Opened = at
	}
	switch {
	case closedStatus(t.Status) && t.Closed.IsZero():
		t.Closed = at
	case !closedStatus(t.Status):
		t.Closed = time.Time{}
	}
	if ok && reflect.DeepEqual(have, t) {
		return
	}
	sp.record(change{at: at, kind: changeTicket, ticket: &t})
}

// Records what a member does when it changed
func (sp *sprint) putLive(at time.Time, name string, st liveState) {
	if have, ok := sp.cur.live[name]; ok && have == st {
		return
	}
	sp.record(change{at: at, kind: changeLive, name: name, live: &st})
}

// Records what a member spent when it changed
func (sp *sprint) putUsage(at time.Time, name string, u Usage) {
	if have, ok := sp.cur.usage[name]; ok && have == u {
		return
	}
	sp.record(change{at: at, kind: changeUsage, name: name, usage: &u})
}

// Records a stage, waiting or paused text when it changed
func (sp *sprint) putText(at time.Time, kind changeKind, text string) {
	var have string
	switch kind {
	case changeStage:
		have = string(sp.cur.stage)
	case changeWaiting:
		have = sp.cur.waiting
	case changePaused:
		have = sp.cur.paused
	}
	if have == text {
		return
	}
	sp.record(change{at: at, kind: kind, text: text})
}

// Ticket id of a key, numbered by kind in the order the keys first came
func (sp *sprint) ticketID(key string, kind TicketKind) string {
	if id, ok := sp.ticketOf[key]; ok {
		return id
	}
	var id string
	if kind == KindTodo {
		sp.todos++
		id = fmt.Sprintf("T%d", sp.todos)
	} else {
		sp.tickets++
		id = fmt.Sprintf("D%d", sp.tickets)
	}
	sp.ticketOf[key] = id
	return id
}

// Folds the main agent into the director, its todo tickets, the stage, the requests and the pause
func (sp *sprint) director(r Report, at time.Time) {
	d, _ := sp.member(directorName)
	d.Task, d.Model, d.Thinking = sp.title, r.Model, r.Thinking
	d.Status = "idle"
	if r.Busy {
		d.Status = "running"
	}
	sp.putMember(at, d)
	spent := Usage{Input: max(r.Usage.Input-sp.base.Input, 0), Output: max(r.Usage.Output-sp.base.Output, 0), Reasoning: max(r.Usage.Reasoning-sp.base.Reasoning, 0), Cost: max(r.Usage.Cost-sp.base.Cost, 0)}
	heat := float64(spent.Input+spent.Output) / nominalTokens
	if r.ContextMax > 0 {
		heat = float64(r.Context) / float64(r.ContextMax)
	}
	sp.putLive(at, directorName, liveState{running: r.Busy, tool: r.Tools.Current, toolAt: r.Tools.Since, approval: r.Approval != "", asking: r.Clarify != "",
		tally: tallyOf(r.Tools.Counts, 0), heat: min(max(heat, 0), 1), since: sp.started, context: r.Context, contextLimit: r.ContextMax, cost: spent.Cost})
	sp.putUsage(at, directorName, spent)
	sp.todoTickets(r, at)
	waiting := ""
	switch {
	case r.Approval != "":
		waiting = "approve " + r.Approval
	case r.Clarify != "":
		waiting = "answer " + r.Clarify
	}
	if waiting != sp.cur.waiting {
		if sp.cur.waiting != "" {
			sp.say(at, MsgAnswer, Person, directorName, "", "answered "+sp.cur.waiting)
		}
		if waiting != "" {
			sp.say(at, MsgQuestion, directorName, Person, "", waiting)
		}
		sp.putText(at, changeWaiting, waiting)
	}
	paused := ""
	if r.Paused {
		paused = "spawning of new agents is paused"
	}
	if paused != sp.cur.paused {
		if paused != "" {
			sp.say(at, MsgChat, Person, "", "", "paused spawning")
		} else {
			sp.say(at, MsgChat, Person, "", "", "resumed spawning")
		}
		sp.putText(at, changePaused, paused)
	}
	stage := sp.cur.stage
	switch {
	case !r.Busy:
	case r.Streaming && r.Tools.Current == "" && sp.crew > 0 && !sp.agentsActive():
		stage = StageReport
	case stage == StageReport && !r.Streaming, stage == StageBrief && (r.Tools.Current != "" || len(r.Tools.Counts) > 0 || r.Thinking != "" || r.Streaming || len(r.Todos) > 0 || waiting != "" || sp.crew > 0):
		stage = StagePlan
	}
	sp.putText(at, changeStage, string(stage))
}

// Folds the todo list of the main agent into tickets the director works, each child a dependency of its parent
func (sp *sprint) todoTickets(r Report, at time.Time) {
	ids := map[string]string{}
	for _, t := range r.Todos {
		ids[t.ID] = sp.ticketID("todo:"+t.ID, KindTodo)
	}
	for _, t := range r.Todos {
		id := ids[t.ID]
		var deps []string
		for _, c := range r.Todos {
			if c.Parent == t.ID && c.ID != t.ID {
				deps = append(deps, ids[c.ID])
			}
		}
		status := todoStatus(t.Status)
		if status == TicketActive && (r.Approval != "" || r.Clarify != "") {
			status = TicketBlocked
		}
		have, known := sp.ticket(id)
		switch {
		case !known:
			sp.say(at, MsgChat, directorName, "", id, "planned "+t.Content)
		case have.Status != status:
			sp.say(at, MsgChat, directorName, "", id, string(status)+" "+t.Content)
		}
		sp.putTicket(at, Ticket{ID: id, Title: t.Content, Kind: KindTodo, Status: status, DependsOn: deps, Builder: directorName})
	}
}

// Ticket status of a todo status
func todoStatus(s string) TicketStatus {
	switch s {
	case "in_progress":
		return TicketActive
	case "completed":
		return TicketDone
	case "cancelled":
		return TicketBlocked
	}
	return TicketTodo
}

// Reports whether a status word means the agent waits or works
func activeStatus(s string) bool { return s == "queued" || s == "running" }

// Reports whether a status word means the agent failed
func failedStatus(s string) bool { return s == "failed" || s == "error" || s == "timeout" }

// Reports whether a ticket status ends its work
func closedStatus(s TicketStatus) bool {
	return s == TicketDone || s == TicketFailed || s == TicketBlocked
}

// Reports whether any agent of the turn still waits or works
func (sp *sprint) agentsActive() bool {
	return slices.ContainsFunc(sp.cur.members, func(m Member) bool { return m.ID != "" && activeStatus(m.Status) })
}

// Reports whether the member named still works, the director while its turn is open
func (sp *sprint) working(name string) bool {
	if name == directorName {
		return sp.turnOpen
	}
	m, ok := sp.member(name)
	return ok && activeStatus(m.Status)
}

// Role of an agent, a reviewer by the work its goal names, an architect once it delegates, else a builder
func roleOf(ag Agent, all []Agent) Role {
	switch {
	case reviewGoal.MatchString(ag.Goal):
		return RoleReviewer
	case slices.ContainsFunc(all, func(o Agent) bool { return o.Parent == ag.ID && o.ID != ag.ID }):
		return RoleArchitect
	}
	return RoleBuilder
}

// Folds one agent into its member, its ticket, its live state and the exchanges its status implies
func (sp *sprint) agent(ag Agent, all []Agent, at time.Time) {
	idx, known := sp.byAgent[ag.ID]
	if !known {
		idx = sp.crew
		sp.crew++
		sp.byAgent[ag.ID] = idx
	}
	name := Config{}.NameOf(RoleBuilder, idx)
	parent := directorName
	if pi, ok := sp.byAgent[ag.Parent]; ok && ag.Parent != "" && ag.Parent != ag.ID {
		parent = Config{}.NameOf(RoleBuilder, pi)
	}
	ticket := sp.ticketID("agent:"+ag.ID, KindDelegation)
	prev, had := sp.member(name)
	thinking := ""
	if n := len(ag.Thinking); n > 0 {
		thinking = ag.Thinking[n-1]
	}
	m := Member{Name: name, Role: roleOf(ag, all), Index: idx, ID: ag.ID, Parent: parent, Task: ag.Goal, Ticket: ticket, Model: ag.Model, Status: ag.Status, Depth: ag.Depth,
		Summary: ag.Summary, Thinking: thinking, Notes: slices.Clone(ag.Notes), Output: slices.Clone(ag.Output), FilesRead: slices.Clone(ag.FilesRead),
		FilesWritten: slices.Clone(ag.FilesWritten), Toolsets: slices.Clone(ag.Toolsets), Started: ag.Started, Duration: ag.Duration}
	sp.putMember(at, m)
	tokens := float64(ag.Usage.Input+ag.Usage.Output) / nominalTokens
	calls := float64(ag.ToolCount) / nominalTools
	sp.putLive(at, name, liveState{running: ag.Status == "running", tool: ag.Tools.Current, toolAt: ag.Tools.Since, failed: failedStatus(ag.Status),
		tally: tallyOf(ag.Tools.Counts, ag.ToolCount), heat: min(max(tokens, calls, 0), 1), since: ag.Started, cost: ag.Usage.Cost})
	sp.putUsage(at, name, ag.Usage)
	var deps []string
	for _, c := range all {
		if c.Parent == ag.ID && c.ID != ag.ID {
			if id, ok := sp.ticketOf["agent:"+c.ID]; ok {
				deps = append(deps, id)
			}
		}
	}
	slices.Sort(deps)
	status := delegationStatus(ag.Status, sp.working(parent))
	before, _ := sp.ticket(ticket)
	sp.putTicket(at, Ticket{ID: ticket, Title: ag.Goal, Kind: KindDelegation, Status: status, DependsOn: deps, Builder: name})
	if before.Status == TicketReview && status == TicketDone {
		sp.accept(at, m, ticket)
	}
	switch was := prev.Status; {
	case !had:
		sp.say(at, MsgAssign, parent, name, ticket, ag.Goal)
		if !activeStatus(ag.Status) {
			sp.finish(at, m, parent, status)
		}
	case !activeStatus(was) && activeStatus(ag.Status):
		sp.say(at, MsgAssign, parent, name, ticket, "again "+ag.Goal)
	case activeStatus(was) && !activeStatus(ag.Status):
		sp.finish(at, m, parent, status)
	}
}

// Messages of an agent that stopped working, its handoff, the verdict of its parent and its failure
func (sp *sprint) finish(at time.Time, m Member, parent string, status TicketStatus) {
	text := m.Summary
	if text == "" {
		text = m.Status
	}
	sp.say(at, MsgHandoff, m.Name, parent, m.Ticket, text)
	switch {
	case failedStatus(m.Status):
		sp.say(at, MsgVerdict, parent, m.Name, m.Ticket, string(VerdictFailed)+": "+m.Status)
		sp.say(at, MsgFailure, m.Name, "", m.Ticket, text)
	case m.Status == "interrupted":
		sp.say(at, MsgVerdict, parent, m.Name, m.Ticket, string(VerdictRework)+": interrupted")
	case status == TicketDone:
		sp.say(at, MsgVerdict, parent, m.Name, m.Ticket, string(VerdictAccepted)+": "+text)
	}
}

// Verdict of the member that asked for a result, accepting it once that member stopped working
func (sp *sprint) accept(at time.Time, m Member, ticket string) {
	text := m.Summary
	if text == "" {
		text = m.Status
	}
	sp.say(at, MsgVerdict, m.Parent, m.Name, ticket, string(VerdictAccepted)+": "+text)
}

// Ends an agent the harness stopped reporting before it told how it ended
func (sp *sprint) vanish(at time.Time, m Member) {
	m.Status = statusGone
	sp.putMember(at, m)
	live := sp.cur.live[m.Name]
	live.running, live.tool = false, ""
	sp.putLive(at, m.Name, live)
	if t, ok := sp.ticket(m.Ticket); ok {
		t.Status = TicketBlocked
		sp.putTicket(at, t)
	}
	sp.say(at, MsgHandoff, m.Name, m.Parent, m.Ticket, "left the roster without a result")
	sp.say(at, MsgVerdict, m.Parent, m.Name, m.Ticket, string(VerdictRework)+": left without a result")
}

// Status of an agent the harness stopped reporting while it still worked
const statusGone = "gone"

// Ticket status of an agent status, a finished result in review while the member that asked for it still works
func delegationStatus(s string, parentWorking bool) TicketStatus {
	switch {
	case s == "queued":
		return TicketTodo
	case s == "running":
		return TicketActive
	case s == "completed" && parentWorking:
		return TicketReview
	case s == "completed":
		return TicketDone
	case s == "interrupted", s == statusGone:
		return TicketBlocked
	}
	return TicketFailed
}

// Ends the turn of the main agent with its outcome, the director idle and its failure told
func (sp *sprint) endTurn(r Report, at time.Time) {
	sp.turnOpen = false
	outcome := r.Outcome
	if outcome == OutcomeRunning {
		outcome = OutcomeStopped
	}
	if outcome == OutcomeDone {
		sp.putText(at, changeStage, string(StageReport))
	}
	failure := ""
	if outcome == OutcomeFailed {
		failure = strings.TrimSpace(r.Failure)
		if failure == "" {
			failure = "the turn failed"
		}
		sp.say(at, MsgFailure, directorName, "", "", failure)
	}
	d, _ := sp.member(directorName)
	d.Status = string(outcome)
	sp.putMember(at, d)
	live := sp.cur.live[directorName]
	live.running, live.tool, live.approval, live.asking, live.failed = false, "", false, false, outcome == OutcomeFailed
	sp.putLive(at, directorName, live)
	if sp.cur.waiting != "" {
		sp.say(at, MsgAnswer, Person, directorName, "", "answered "+sp.cur.waiting)
		sp.putText(at, changeWaiting, "")
	}
	sp.record(change{at: at, kind: changeRun, running: sp.agentsActive(), outcome: outcome, text: failure})
}

// Accepts every result in review whose member that asked for it stopped working, and marks the turn running or ended
func (sp *sprint) settle(at time.Time) {
	for _, t := range slices.Clone(sp.cur.tickets) {
		if t.Status != TicketReview {
			continue
		}
		m, ok := sp.member(t.Builder)
		if !ok || sp.working(m.Parent) {
			continue
		}
		t.Status = TicketDone
		sp.putTicket(at, t)
		sp.accept(at, m, t.ID)
	}
	running := sp.turnOpen || sp.agentsActive()
	if running != sp.cur.running {
		sp.record(change{at: at, kind: changeRun, running: running, outcome: sp.cur.outcome, text: sp.cur.failure})
	}
}

// Tool starts by class, the calls no name reached counted as other tools
func tallyOf(counts map[string]int, total int) toolTally {
	var t toolTally
	for name, n := range counts {
		t.addN(name, n)
	}
	if gap := total - t.total(); gap > 0 {
		t.others += gap
	}
	return t
}
