package sprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
)

// List of names that also reads one string, split on newlines and commas, as models often send it
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

// Keymap context of the board and the actions it answers
const (
	Context        = "sprint"
	ActionOpen     = "sprint.open"
	ActionStop     = "sprint.stop"
	ActionResume   = "sprint.resume"
	ActionSettings = "sprint.settings"
	// Keys of the board panel and the settings panel
	BoardKey    = "sprints"
	SettingsKey = "sprint-settings"
)

// Stage a sprint is in
type Stage string

const (
	StageRefine Stage = "refine"
	StagePlan   Stage = "plan"
	StageBrief  Stage = "brief"
	StageBuild  Stage = "build"
	StageReview Stage = "review"
	StageReport Stage = "report"
	StageDone   Stage = "done"
	StageFailed Stage = "failed"
)

// Where a ticket stands
type TicketStatus string

const (
	TicketTodo     TicketStatus = "todo"
	TicketActive   TicketStatus = "active"
	TicketReported TicketStatus = "reported"
	TicketDone     TicketStatus = "done"
	TicketRework   TicketStatus = "rework"
	TicketFailed   TicketStatus = "failed"
)

// What an engineer reports to the lead about a ticket
const (
	ReportDone     = "done"
	ReportBlocked  = "blocked"
	ReportQuestion = "question"
)

var reportStatuses = []string{ReportDone, ReportBlocked, ReportQuestion}

// What the lead decides on a report
const (
	VerdictAccepted = "accepted"
	VerdictRework   = "rework"
	VerdictGuidance = "guidance"
	VerdictFailed   = "failed"
)

var verdicts = []string{VerdictAccepted, VerdictRework, VerdictGuidance, VerdictFailed}

// What the project manager settled with the person
type Objective struct {
	Objective   string  `json:"objective"`
	Acceptance  Strings `json:"acceptance,omitempty"`
	Constraints Strings `json:"constraints,omitempty"`
	Notes       string  `json:"notes,omitempty"`
}

// Sized unit of work the planner cut the objective into
type Story struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
	// Size in the words of the planner, such as small, 5 points or a day
	Size string `json:"size,omitempty"`
}

// One piece of the story an engineer takes
type Ticket struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Files       Strings `json:"files,omitempty"`
	DependsOn   Strings `json:"depends_on,omitempty"`
	Estimate    string  `json:"estimate,omitempty"`
	Status      TicketStatus
	// Member working or last at work on it
	Engineer string
	// Last report the engineer made on it
	Report string
	// Notes of the lead that sent it back or guided it, oldest first
	Notes []string
	// Why the last attempt failed, empty when it did not
	Err string
	// Times the lead sent it back, by direction or review
	Round int
}

// One session of the team
type Member struct {
	Role string
	// Name such as pm, planner, lead or engineer-2
	Name    string
	Live    *harness.Live
	Started time.Time
}

// Report an engineer handed the lead, answered with the direction of the lead
type report struct {
	Ticket   string
	Engineer string
	Status   string
	Text     string
	Filed    time.Time
	reply    chan direction
}

// What the lead decided on a report
type direction struct {
	Verdict string
	Notes   string
}

// One sprint from the prompt that started it to its end
type Sprint struct {
	mu       sync.Mutex
	ID       string
	Prompt   string
	Started  time.Time
	Finished time.Time
	Stage    Stage
	// Stage the sprint was in when it ended, done once it finished its work
	Last      Stage
	Status    string
	Err       error
	Objective Objective
	Story     Story
	// What the lead told the engineers before the build
	Briefing string
	Tickets  []*Ticket
	Members  []Member
	// Review rounds done so far
	Round int
	// What the lead reported at the end
	Summary string
	// Member whose session the sprint waits on the person to drive, empty while it waits on none
	Waiting string
	// Why the sprint waits for the person to tell it to go on, empty while it runs on its own
	Paused string
	// Signal the person gave a paused sprint to go on
	resume chan struct{}
	// Reports on their way to the lead
	inbox chan *report
	// Reports waiting for the direction of the lead, by ticket id
	open map[string]*report
	// Count of directions the lead has given
	directed int
	// Why the lead can direct no more this sprint, empty while it can
	leadGone string
	// The build round ran past its limit, so the lead may only accept or fail
	lateRound atomic.Bool
}

// Copy of a sprint safe to read while the workflow goes on
type Snapshot struct {
	ID        string
	Prompt    string
	Started   time.Time
	Finished  time.Time
	Stage     Stage
	Last      Stage
	Status    string
	Err       error
	Objective Objective
	Story     Story
	Briefing  string
	Tickets   []Ticket
	Members   []Member
	Round     int
	Summary   string
	Waiting   string
	Paused    string
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

func newSprint(id, prompt string, started time.Time) *Sprint {
	return &Sprint{ID: id, Prompt: prompt, Started: started, Stage: StageRefine, Status: "starting", inbox: make(chan *report, 64), open: map[string]*report{}}
}

func (s *Sprint) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Snapshot{ID: s.ID, Prompt: s.Prompt, Started: s.Started, Finished: s.Finished, Stage: s.Stage, Last: s.Last, Status: s.Status, Err: s.Err, Objective: s.Objective, Story: s.Story, Briefing: s.Briefing, Round: s.Round, Summary: s.Summary, Waiting: s.Waiting, Paused: s.Paused}
	out.Tickets = make([]Ticket, 0, len(s.Tickets))
	for _, t := range s.Tickets {
		c := *t
		c.Files = append(Strings(nil), t.Files...)
		c.DependsOn = append(Strings(nil), t.DependsOn...)
		c.Notes = append([]string(nil), t.Notes...)
		out.Tickets = append(out.Tickets, c)
	}
	out.Members = append([]Member(nil), s.Members...)
	return out
}

// Moves the sprint to a stage with the words shown beside it
func (s *Sprint) enter(stage Stage, status string) {
	s.mu.Lock()
	s.Stage, s.Status = stage, status
	s.mu.Unlock()
}

func (s *Sprint) setStatus(status string) {
	s.mu.Lock()
	s.Status = status
	s.mu.Unlock()
}

// Names the member the sprint waits on the person to drive, empty once it waits on none
func (s *Sprint) setWaiting(name string) {
	s.mu.Lock()
	s.Waiting = name
	s.mu.Unlock()
}

// Records why the sprint waits for the person to tell it to go on
func (s *Sprint) pause(why string) {
	s.mu.Lock()
	s.Paused = why
	s.mu.Unlock()
}

// Clears the pause and the member waited on once the sprint goes on
func (s *Sprint) unpause() {
	s.mu.Lock()
	s.Paused, s.Waiting = "", ""
	s.mu.Unlock()
}

// Channel a go on signal arrives on
func (s *Sprint) resumed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resume == nil {
		s.resume = make(chan struct{}, 1)
	}
	return s.resume
}

// Tells a waiting sprint to go on, false when it is not waiting for the person
func (s *Sprint) Resume() bool {
	s.mu.Lock()
	waiting := s.Finished.IsZero() && (s.Paused != "" || s.Waiting != "")
	if s.resume == nil {
		s.resume = make(chan struct{}, 1)
	}
	ch := s.resume
	s.mu.Unlock()
	if !waiting {
		return false
	}
	select {
	case ch <- struct{}{}:
	default:
	}
	return true
}

func (s *Sprint) setObjective(o Objective) {
	s.mu.Lock()
	s.Objective = o
	s.mu.Unlock()
}

func (s *Sprint) setPlan(story Story, tickets []*Ticket) {
	s.mu.Lock()
	s.Story, s.Tickets = story, tickets
	s.mu.Unlock()
}

func (s *Sprint) setBriefing(text string) {
	s.mu.Lock()
	s.Briefing = text
	s.mu.Unlock()
}

// Adds a member, taking the place of an earlier one of the same name whose session was closed
func (s *Sprint) putMember(m Member) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, have := range s.Members {
		if have.Name == m.Name {
			s.Members[i] = m
			return
		}
	}
	s.Members = append(s.Members, m)
}

// Drops the members whose sessions are no longer open, keep reporting which are
func (s *Sprint) pruneMembers(open func(m Member) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.Members[:0]
	for _, m := range s.Members {
		if open(m) {
			kept = append(kept, m)
		}
	}
	s.Members = kept
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

// Status of a ticket right now
func (s *Sprint) statusOf(t *Ticket) TicketStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return t.Status
}

// Runs fn on a ticket under the lock
func (s *Sprint) update(t *Ticket, fn func(t *Ticket)) {
	s.mu.Lock()
	fn(t)
	s.mu.Unlock()
}

// Files a report on a ticket for the lead, refusing one on a ticket already waiting or not the engineer's
func (s *Sprint) file(ctx context.Context, r *report) error {
	if err := s.accept(r); err != nil {
		return err
	}
	select {
	case s.inbox <- r:
		return nil
	case <-ctx.Done():
		s.withdraw(r)
		return ctx.Err()
	}
}

// Marks the ticket of a report as waiting on the lead, refusing a report the sprint cannot take
func (s *Sprint) accept(r *report) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.find(r.Ticket)
	switch {
	case s.leadGone != "":
		return fmt.Errorf("nobody can direct you, %s", s.leadGone)
	case t == nil:
		return fmt.Errorf("no ticket %s in sprint %s", r.Ticket, s.ID)
	case t.Engineer != r.Engineer:
		return fmt.Errorf("ticket %s is not yours, it belongs to %s", r.Ticket, t.Engineer)
	case t.Status == TicketReported:
		return fmt.Errorf("a report on ticket %s already waits for the lead", r.Ticket)
	case t.Status != TicketActive:
		return fmt.Errorf("ticket %s is %s, nothing is left to report on it", r.Ticket, t.Status)
	}
	t.Status, t.Report = TicketReported, r.Text
	s.open[r.Ticket] = r
	return nil
}

// Takes back a report the lead never answered, the ticket going on as active, false when the lead already answered
func (s *Sprint) withdraw(r *report) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open[r.Ticket] != r {
		return false
	}
	delete(s.open, r.Ticket)
	if t := s.find(r.Ticket); t != nil && t.Status == TicketReported {
		t.Status = TicketActive
	}
	return true
}

// Answers the report waiting on a ticket with the direction of the lead, moving the ticket as the verdict says
func (s *Sprint) direct(ticket string, d direction, maxRework int) (*report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.open[ticket]
	if !ok {
		if len(s.open) == 0 {
			return nil, fmt.Errorf("no engineer waits for direction on ticket %s, no report is open", ticket)
		}
		return nil, fmt.Errorf("no engineer waits for direction on ticket %s, reports are open on %s", ticket, strings.Join(s.openIDs(), ", "))
	}
	t := s.find(ticket)
	switch d.Verdict {
	case VerdictRework:
		if s.lateRound.Load() {
			return nil, fmt.Errorf("the build round is past its limit, accept ticket %s or fail it", ticket)
		}
		if t.Round >= maxRework {
			return nil, fmt.Errorf("ticket %s was sent back %d times, the most the sprint allows, accept it or fail it", ticket, t.Round)
		}
		t.Round++
		t.Notes = append(t.Notes, d.Notes)
		t.Status = TicketActive
	case VerdictGuidance:
		t.Notes = append(t.Notes, d.Notes)
		t.Status = TicketActive
	case VerdictAccepted:
		if r.Status != ReportDone {
			return nil, fmt.Errorf("ticket %s was reported %s, not done, give guidance or fail it", ticket, r.Status)
		}
		t.Status, t.Err = TicketDone, ""
	case VerdictFailed:
		t.Status, t.Err = TicketFailed, "failed by the lead: "+d.Notes
	}
	delete(s.open, ticket)
	s.directed++
	r.reply <- d
	return r, nil
}

// Fails every open report and refuses later ones, for a lead that cannot direct any more, reporting how many failed
func (s *Sprint) loseLead(why string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leadGone = why
	n := 0
	for id, r := range s.open {
		if t := s.find(id); t != nil {
			t.Status, t.Err = TicketFailed, why
		}
		delete(s.open, id)
		r.reply <- direction{Verdict: VerdictFailed, Notes: why}
		n++
	}
	return n
}

// Reports waiting for the lead, oldest first
func (s *Sprint) openReports() []*report {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*report, 0, len(s.open))
	for _, r := range s.open {
		out = append(out, r)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Filed.Before(out[j-1].Filed); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Ids of the tickets with a report waiting, in id order
func (s *Sprint) openIDs() []string {
	var out []string
	for _, t := range s.Tickets {
		if _, ok := s.open[t.ID]; ok {
			out = append(out, t.ID)
		}
	}
	return out
}

// Count of directions the lead has given so far
func (s *Sprint) directions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.directed
}

// Ends the sprint, failed when err is set and done otherwise, remembering the stage it was in
func (s *Sprint) finish(err error, summary string) {
	s.mu.Lock()
	s.Finished, s.Err, s.Last = time.Now(), err, s.Stage
	if summary != "" {
		s.Summary = summary
	}
	if err != nil {
		s.Stage, s.Status = StageFailed, err.Error()
	} else {
		s.Stage, s.Last, s.Status = StageDone, StageDone, "done"
	}
	s.mu.Unlock()
}

// Readies an ended sprint to run again from the stage it was in, tickets cut short going back to the queue
func (s *Sprint) restart() Stage {
	from := restartStage(s.Snapshot())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Finished, s.Err, s.Stage, s.Last, s.Status = time.Time{}, nil, from, "", "restarting"
	s.Paused, s.Waiting, s.leadGone = "", "", ""
	s.lateRound.Store(false)
	for id, r := range s.open {
		delete(s.open, id)
		r.reply <- direction{Verdict: VerdictFailed, Notes: "the sprint restarted"}
	}
	for _, t := range s.Tickets {
		switch {
		case t.Status == TicketActive || t.Status == TicketReported:
			t.Status, t.Err = TicketTodo, ""
		case t.Status == TicketFailed && !strings.HasPrefix(t.Err, "failed by the lead"):
			t.Status, t.Err = TicketTodo, ""
		}
	}
	return from
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
func sprintOfSession(id string) (*Sprint, Member, bool) {
	regMu.Lock()
	list := append([]*Sprint(nil), sprints...)
	regMu.Unlock()
	for i := len(list) - 1; i >= 0; i-- {
		for _, m := range list[i].Snapshot().Members {
			if m.Live != nil && m.Live.Session != nil && m.Live.Session.ID() == id {
				return list[i], m, true
			}
		}
	}
	return nil, Member{}, false
}

// Every sprint started in this process, newest first
func Sprints() []Snapshot {
	regMu.Lock()
	list := append([]*Sprint(nil), sprints...)
	regMu.Unlock()
	out := make([]Snapshot, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		out = append(out, list[i].Snapshot())
	}
	return out
}
