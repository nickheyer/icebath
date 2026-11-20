package arena

import (
	"slices"
	"time"
)

// Name of the arena as the slash command and the card key spell it
const Name = "arena"

// Part a member plays on the platform
type Role string

const (
	RoleDirector  Role = "director"
	RoleArchitect Role = "architect"
	RoleBuilder   Role = "builder"
	RoleReviewer  Role = "reviewer"
)

// State of a ticket on the board
type TicketStatus string

const (
	TicketTodo    TicketStatus = "todo"
	TicketActive  TicketStatus = "active"
	TicketReview  TicketStatus = "review"
	TicketDone    TicketStatus = "done"
	TicketFailed  TicketStatus = "failed"
	TicketBlocked TicketStatus = "blocked"
)

// Where a ticket came from, a delegated goal or a todo item of the director
type TicketKind string

const (
	KindDelegation TicketKind = "delegation"
	KindTodo       TicketKind = "todo"
)

// Phase of a turn, the prompt arriving, the work and the final answer
type Stage string

const (
	StageBrief  Stage = "brief"
	StagePlan   Stage = "plan"
	StageReport Stage = "report"
)

// Kind of exchange between members, each shown its own way on the platform
type MsgKind string

const (
	MsgAssign   MsgKind = "assign"
	MsgHandoff  MsgKind = "handoff"
	MsgQuestion MsgKind = "question"
	MsgAnswer   MsgKind = "answer"
	MsgVerdict  MsgKind = "verdict"
	MsgFailure  MsgKind = "failure"
	MsgChat     MsgKind = "chat"
)

// Judgement a parent passes on the work of a member, the leading word of a verdict message
type Verdict string

const (
	VerdictAccepted Verdict = "accepted"
	VerdictRework   Verdict = "rework"
	VerdictFailed   Verdict = "failed"
	VerdictGuidance Verdict = "guidance"
)

// Name the person at the terminal goes by in messages
const Person = "you"

// Addressee of a message meant for every member
const Team = "team"

// One member of the crew, the main agent or a delegated subagent
type Member struct {
	Name         string
	Role         Role
	Index        int
	ID           string
	Parent       string
	Task         string
	Ticket       string
	Model        string
	Status       string
	Depth        int
	Summary      string
	Thinking     string
	Notes        []string
	Output       []string
	FilesRead    []string
	FilesWritten []string
	Toolsets     []string
	Started      time.Time
	Duration     time.Duration
}

// One unit of work on the board, a delegated goal or a todo item
type Ticket struct {
	ID        string
	Title     string
	Kind      TicketKind
	Status    TicketStatus
	DependsOn []string
	Builder   string
	Round     int
	Opened    time.Time
	Closed    time.Time
}

// One exchange the platform shows, numbered in the order it happened
type Message struct {
	Seq    int
	At     time.Time
	Kind   MsgKind
	From   string
	To     string
	Ticket string
	Text   string
}

// Tokens and dollars a member has spent
type Usage struct {
	Input     int
	Output    int
	Reasoning int
	Cost      float64
}

// How a turn ended
type Outcome string

const (
	OutcomeRunning Outcome = ""
	OutcomeDone    Outcome = "done"
	OutcomeFailed  Outcome = "failed"
	OutcomeStopped Outcome = "stopped"
)

// State of one turn of a session at one moment, everything the platform and the dock show
type Snapshot struct {
	ID       string
	Sprint   string
	Turn     int
	Title    string
	Stage    Stage
	Started  time.Time
	Ended    time.Time
	At       time.Time
	Members  []Member
	Tickets  []Ticket
	Messages []Message
	Usage    map[string]Usage
	Waiting  string
	Paused   string
	Err      error
	Outcome  Outcome
	Live     bool
	Newest   bool
	Since    int
}

// Reports whether the turn or any of its agents still works
func (s *Snapshot) Running() bool { return s != nil && s.Live }

// Reports whether the turn was stopped before it finished
func (s *Snapshot) Stopped() bool { return s != nil && !s.Live && s.Outcome == OutcomeStopped }

// Reports whether a retry would run the prompt of this turn again, its newest turn having failed or stopped
func (s *Snapshot) Restartable() bool {
	return s != nil && !s.Live && s.Newest && (s.Outcome == OutcomeFailed || s.Outcome == OutcomeStopped)
}

// Ticket with an id
func (s *Snapshot) Ticket(id string) (Ticket, bool) {
	if s == nil || id == "" {
		return Ticket{}, false
	}
	i := slices.IndexFunc(s.Tickets, func(t Ticket) bool { return t.ID == id })
	if i < 0 {
		return Ticket{}, false
	}
	return s.Tickets[i], true
}

// Member with a name
func (s *Snapshot) Member(name string) (Member, bool) {
	if s == nil || name == "" {
		return Member{}, false
	}
	i := slices.IndexFunc(s.Members, func(m Member) bool { return m.Name == name })
	if i < 0 {
		return Member{}, false
	}
	return s.Members[i], true
}

// Messages numbered past seq, oldest first
func (s *Snapshot) MessagesAfter(seq int) []Message {
	if s == nil {
		return nil
	}
	i, _ := slices.BinarySearchFunc(s.Messages, seq, func(m Message, n int) int { return m.Seq - n - 1 })
	return s.Messages[i:]
}

// Reports whether any ticket waits in review
func (s *Snapshot) inReview() bool {
	return s != nil && slices.ContainsFunc(s.Tickets, func(t Ticket) bool { return t.Status == TicketReview })
}
