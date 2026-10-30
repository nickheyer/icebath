package sprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

// Tools the roles hand their work on with
const (
	ToolResolve = "SprintResolve"
	ToolPlan    = "SprintPlan"
	ToolReview  = "SprintReview"
)

// Stage waiting for one tool call on one session
type wait struct {
	tool string
	ch   chan any
}

var waits struct {
	mu sync.Mutex
	m  map[string]*wait
}

// Starts waiting for the tool to be called from the session, the earlier wait on that session replaced
func await(sessionID, toolName string) *wait {
	w := &wait{tool: toolName, ch: make(chan any, 1)}
	waits.mu.Lock()
	if waits.m == nil {
		waits.m = map[string]*wait{}
	}
	waits.m[sessionID] = w
	waits.mu.Unlock()
	return w
}

// Stops waiting on the session when the wait is still the one registered
func (w *wait) release(sessionID string) {
	waits.mu.Lock()
	if waits.m[sessionID] == w {
		delete(waits.m, sessionID)
	}
	waits.mu.Unlock()
}

// Hands a tool call to the stage waiting for it on the session, failing when none waits for that tool
func deliver(sessionID, toolName string, v any) error {
	waits.mu.Lock()
	w := waits.m[sessionID]
	waits.mu.Unlock()
	if w == nil {
		return errors.New(unwaited(sessionID, toolName))
	}
	if w.tool != toolName {
		return fmt.Errorf("this stage of the sprint waits for %s, not %s", w.tool, toolName)
	}
	select {
	case w.ch <- v:
		return nil
	default:
		return fmt.Errorf("%s was already called for this stage", toolName)
	}
}

// Why no stage waits for the tool on the session, by what became of the sprint the session served
func unwaited(sessionID, toolName string) string {
	snap, ok := sprintOfSession(sessionID)
	if !ok {
		return fmt.Sprintf("this session is not part of a sprint, so nothing waits for %s; the person starts one with /%s <request>", toolName, Name)
	}
	if snap.Running() {
		return fmt.Sprintf("sprint %s is in its %s stage and is not waiting for %s on this session", snap.ID, snap.Stage, toolName)
	}
	why := "done"
	if snap.Err != nil {
		why = "failed: " + snap.Err.Error()
	}
	return fmt.Sprintf("sprint %s ended %s ago (%s), so nothing waits for %s on this session; the person can start another with /%s <request>", snap.ID, span(time.Since(snap.Finished)), why, toolName, Name)
}

// Tools of the sprint roles, each handing its call to the stage waiting on the session of the calling run
func sprintTools(app *harness.App) []tool.Tool {
	return []tool.Tool{resolveTool{app: app}, planTool{app: app}, reviewTool{app: app}}
}

// Session the calling run goes on, so the stage waiting on it receives the call
func sessionOf(app *harness.App, ctx context.Context) (string, error) {
	sess := app.SessionFor(ctx)
	if sess == nil {
		return "", fmt.Errorf("no session is running this call")
	}
	return sess.ID(), nil
}

type resolveTool struct{ app *harness.App }

func (resolveTool) Definition() protocol.Tool {
	return protocol.Tool{Name: ToolResolve, Description: "Hands the settled objective of the sprint to the planner. Call it once, when nothing that would change what gets built is still open.", Parameters: json.RawMessage(`{"type":"object","properties":{
"objective":{"type":"string","description":"What the team builds, in enough detail to build it without asking"},
"acceptance":{"type":"array","items":{"type":"string"},"description":"Checks that show the objective is met"},
"constraints":{"type":"array","items":{"type":"string"},"description":"What must stay as it is or be avoided"},
"notes":{"type":"string","description":"Choices made where the person was silent, and anything else the team should know"}},
"required":["objective"],"additionalProperties":false}`)}
}

func (resolveTool) Annotations() tool.Annotations { return tool.Annotations{Idempotent: true} }

func (t resolveTool) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var o Objective
	if err := json.Unmarshal(args, &o); err != nil {
		return tool.Error(err.Error()), nil
	}
	if strings.TrimSpace(o.Objective) == "" {
		return tool.Error("objective is required"), nil
	}
	id, err := sessionOf(t.app, ctx)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if err := deliver(id, ToolResolve, o); err != nil {
		return tool.Error(err.Error()), nil
	}
	return tool.Text("Objective handed to the planner. Tell the person the sprint is under way, then wait to be told the outcome.\n\n" + objectiveText(o)).With(o), nil
}

// Story and tickets as the planner submits them
type planInput struct {
	Story   Story    `json:"story"`
	Tickets []Ticket `json:"tickets"`
}

type planTool struct{ app *harness.App }

func (planTool) Definition() protocol.Tool {
	return protocol.Tool{Name: ToolPlan, Description: "Submits the sized story and its tickets to the sprint team. Call it once with every ticket.", Parameters: json.RawMessage(`{"type":"object","properties":{
"story":{"type":"object","properties":{
  "title":{"type":"string"},
  "summary":{"type":"string","description":"The change as a whole"},
  "size":{"type":"string","description":"How big it is, in your own units"}},
  "required":["title","summary"],"additionalProperties":false},
"tickets":{"type":"array","items":{"type":"object","properties":{
  "id":{"type":"string","description":"Short id such as T1, given in order when omitted"},
  "title":{"type":"string"},
  "description":{"type":"string","description":"What to build, enough for an engineer who has not read the objective"},
  "files":{"type":"array","items":{"type":"string"},"description":"Files the ticket owns, which no other ticket edits"},
  "depends_on":{"type":"array","items":{"type":"string"},"description":"Ids of tickets that must be done first"},
  "estimate":{"type":"string"}},
  "required":["title","description"],"additionalProperties":false}}},
"required":["story","tickets"],"additionalProperties":false}`)}
}

func (planTool) Annotations() tool.Annotations { return tool.Annotations{Idempotent: true} }

func (t planTool) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in planInput
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	if err := checkPlan(&in, Current().Team.MaxTickets); err != nil {
		return tool.Error(err.Error()), nil
	}
	id, err := sessionOf(t.app, ctx)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if err := deliver(id, ToolPlan, in); err != nil {
		return tool.Error(err.Error()), nil
	}
	return tool.Text(fmt.Sprintf("Story %q with %d tickets handed to the team.\n\n", in.Story.Title, len(in.Tickets)) + planText(in.Story, in.Tickets)).With(in), nil
}

// Fills in missing ids, then refuses an empty plan, too many tickets, duplicate ids, unknown dependencies or a cycle
func checkPlan(in *planInput, maxTickets int) error {
	if strings.TrimSpace(in.Story.Title) == "" || strings.TrimSpace(in.Story.Summary) == "" {
		return fmt.Errorf("story needs a title and a summary")
	}
	if len(in.Tickets) == 0 {
		return fmt.Errorf("at least one ticket is required")
	}
	if len(in.Tickets) > maxTickets {
		return fmt.Errorf("%d tickets is more than the %d the sprint allows, merge some", len(in.Tickets), maxTickets)
	}
	ids := map[string]bool{}
	for i := range in.Tickets {
		t := &in.Tickets[i]
		t.ID = strings.TrimSpace(t.ID)
		if t.ID == "" {
			t.ID = fmt.Sprintf("T%d", i+1)
		}
		if strings.TrimSpace(t.Title) == "" || strings.TrimSpace(t.Description) == "" {
			return fmt.Errorf("ticket %s needs a title and a description", t.ID)
		}
		if ids[t.ID] {
			return fmt.Errorf("ticket id %s is used twice", t.ID)
		}
		ids[t.ID] = true
		t.Status = TicketTodo
	}
	for _, t := range in.Tickets {
		for _, d := range t.DependsOn {
			if !ids[d] {
				return fmt.Errorf("ticket %s depends on unknown ticket %s", t.ID, d)
			}
			if d == t.ID {
				return fmt.Errorf("ticket %s depends on itself", t.ID)
			}
		}
	}
	if cycle := dependencyCycle(in.Tickets); cycle != "" {
		return fmt.Errorf("tickets depend on each other in a loop through %s", cycle)
	}
	return nil
}

// Id of a ticket inside a dependency loop, empty when the tickets order
func dependencyCycle(tickets []Ticket) string {
	deps := map[string][]string{}
	for _, t := range tickets {
		deps[t.ID] = t.DependsOn
	}
	const (
		open = iota + 1
		closed
	)
	state := map[string]int{}
	var visit func(id string) bool
	visit = func(id string) bool {
		switch state[id] {
		case open:
			return true
		case closed:
			return false
		}
		state[id] = open
		for _, d := range deps[id] {
			if visit(d) {
				return true
			}
		}
		state[id] = closed
		return false
	}
	for _, t := range tickets {
		if visit(t.ID) {
			return t.ID
		}
	}
	return ""
}

// Verdict of the lead on one ticket
type verdict struct {
	Ticket string `json:"ticket"`
	Status string `json:"status"`
	Notes  string `json:"notes,omitempty"`
}

// Review as the lead submits it
type reviewInput struct {
	Verdicts []verdict `json:"verdicts"`
	Summary  string    `json:"summary"`
}

// Count of tickets the review sent back
func (in reviewInput) rework() int {
	n := 0
	for _, v := range in.Verdicts {
		if v.Status == "rework" {
			n++
		}
	}
	return n
}

type reviewTool struct{ app *harness.App }

func (reviewTool) Definition() protocol.Tool {
	return protocol.Tool{Name: ToolReview, Description: "Closes a review round with a verdict for every ticket and a summary of the work. Rework sends the ticket back to its engineer with the notes.", Parameters: json.RawMessage(`{"type":"object","properties":{
"verdicts":{"type":"array","items":{"type":"object","properties":{
  "ticket":{"type":"string","description":"Ticket id"},
  "status":{"type":"string","enum":["accepted","rework"]},
  "notes":{"type":"string","description":"For rework, exactly what is wrong and what done looks like"}},
  "required":["ticket","status"],"additionalProperties":false}},
"summary":{"type":"string","description":"State of the work as a whole, what was built, how it was verified and what the person should know"}},
"required":["verdicts","summary"],"additionalProperties":false}`)}
}

func (reviewTool) Annotations() tool.Annotations { return tool.Annotations{Idempotent: true} }

func (t reviewTool) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in reviewInput
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	if strings.TrimSpace(in.Summary) == "" {
		return tool.Error("summary is required"), nil
	}
	for _, v := range in.Verdicts {
		if v.Status != "accepted" && v.Status != "rework" {
			return tool.Errorf("ticket %s: status must be accepted or rework, got %q", v.Ticket, v.Status), nil
		}
		if v.Status == "rework" && strings.TrimSpace(v.Notes) == "" {
			return tool.Errorf("ticket %s: rework needs notes", v.Ticket), nil
		}
	}
	id, err := sessionOf(t.app, ctx)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if err := deliver(id, ToolReview, in); err != nil {
		return tool.Error(err.Error()), nil
	}
	return tool.Text(fmt.Sprintf("Review recorded, %d sent back.\n\n", in.rework()) + reviewText(in)).With(in), nil
}
