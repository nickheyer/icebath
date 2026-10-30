package sprint

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

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
	StageBuild  Stage = "build"
	StageReview Stage = "review"
	StageReport Stage = "report"
	StageDone   Stage = "done"
	StageFailed Stage = "failed"
)

// Where a ticket stands
type TicketStatus string

const (
	TicketTodo   TicketStatus = "todo"
	TicketActive TicketStatus = "active"
	TicketDone   TicketStatus = "done"
	TicketRework TicketStatus = "rework"
	TicketFailed TicketStatus = "failed"
)

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
	// What the engineer reported when done
	Report string
	// Review notes that sent it back, oldest first
	Notes []string
	// Why the last attempt failed, empty when it did not
	Err string
	// Review rounds it went through
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

// One sprint from the prompt that started it to its end
type Sprint struct {
	mu        sync.Mutex
	ID        string
	Prompt    string
	Started   time.Time
	Finished  time.Time
	Stage     Stage
	Status    string
	Err       error
	Objective Objective
	Story     Story
	Tickets   []*Ticket
	Members   []Member
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
}

// Copy of a sprint safe to read while the workflow goes on
type Snapshot struct {
	ID        string
	Prompt    string
	Started   time.Time
	Finished  time.Time
	Stage     Stage
	Status    string
	Err       error
	Objective Objective
	Story     Story
	Tickets   []Ticket
	Members   []Member
	Round     int
	Summary   string
	Waiting   string
	Paused    string
}

// Reports whether the sprint still goes
func (s Snapshot) Running() bool { return s.Finished.IsZero() }

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

func (s *Sprint) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Snapshot{ID: s.ID, Prompt: s.Prompt, Started: s.Started, Finished: s.Finished, Stage: s.Stage, Status: s.Status, Err: s.Err, Objective: s.Objective, Story: s.Story, Round: s.Round, Summary: s.Summary, Waiting: s.Waiting, Paused: s.Paused}
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

func (s *Sprint) addMember(m Member) {
	s.mu.Lock()
	s.Members = append(s.Members, m)
	s.mu.Unlock()
}

// Ticket by id, nil for an unknown one
func (s *Sprint) ticket(id string) *Ticket {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.Tickets {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// Runs fn on a ticket under the lock
func (s *Sprint) update(t *Ticket, fn func(t *Ticket)) {
	s.mu.Lock()
	fn(t)
	s.mu.Unlock()
}

// Ends the sprint, failed when err is set and done otherwise
func (s *Sprint) finish(err error, summary string) {
	s.mu.Lock()
	s.Finished, s.Err = time.Now(), err
	if summary != "" {
		s.Summary = summary
	}
	if err != nil {
		s.Stage, s.Status = StageFailed, err.Error()
	} else {
		s.Stage, s.Status = StageDone, "done"
	}
	s.mu.Unlock()
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

// Sprint a session belongs to as a member, false for a session of no sprint
func sprintOfSession(id string) (Snapshot, bool) {
	regMu.Lock()
	list := append([]*Sprint(nil), sprints...)
	regMu.Unlock()
	for i := len(list) - 1; i >= 0; i-- {
		snap := list[i].Snapshot()
		for _, m := range snap.Members {
			if m.Live != nil && m.Live.Session != nil && m.Live.Session.ID() == id {
				return snap, true
			}
		}
	}
	return Snapshot{}, false
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
