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
	ToolAsk     = "SprintAsk"
	ToolReport  = "SprintReport"
	ToolDirect  = "SprintDirect"
	ToolReview  = "SprintReview"
)

// Most questions one ask puts and most choices one question offers
const (
	maxQuestions = 4
	maxChoices   = 4
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
	s, _, ok := sprintOfSession(sessionID)
	if !ok {
		return fmt.Sprintf("this session is not part of a sprint, so nothing waits for %s; the person starts one with /%s <request>", toolName, Name)
	}
	snap := s.Snapshot()
	if snap.Running() {
		return fmt.Sprintf("sprint %s is in its %s stage and is not waiting for %s on this session", snap.ID, snap.Stage, toolName)
	}
	why := "done"
	if snap.Err != nil {
		why = "failed: " + snap.Err.Error()
	}
	return fmt.Sprintf("sprint %s ended %s ago (%s), so nothing waits for %s on this session; the person can start another with /%s <request>", snap.ID, span(time.Since(snap.Finished)), why, toolName, Name)
}

// Tools of the sprint roles, each handing its call to the sprint the session of the calling run serves
func sprintTools(app *harness.App) []tool.Tool {
	return []tool.Tool{resolveTool{app: app}, planTool{app: app}, askTool{app: app}, reportTool{app: app}, directTool{app: app}, reviewTool{app: app}}
}

// Session the calling run goes on, so the stage waiting on it receives the call
func sessionOf(app *harness.App, ctx context.Context) (string, error) {
	sess := app.SessionFor(ctx)
	if sess == nil {
		return "", fmt.Errorf("no session is running this call")
	}
	return sess.ID(), nil
}

// Running sprint the calling run serves in the role, with the member, refusing another role or no sprint
func memberOf(app *harness.App, ctx context.Context, role string) (*Sprint, Member, error) {
	id, err := sessionOf(app, ctx)
	if err != nil {
		return nil, Member{}, err
	}
	s, m, ok := sprintOfSession(id)
	if !ok {
		return nil, Member{}, fmt.Errorf("this session is not part of a sprint; the person starts one with /%s <request>", Name)
	}
	if !s.Snapshot().Running() {
		return nil, Member{}, errors.New(unwaited(id, roleTool(role)))
	}
	if m.Role != role {
		return nil, Member{}, fmt.Errorf("%s is for the %s of the sprint, this session is its %s", roleTool(role), role, m.Name)
	}
	return s, m, nil
}

// Tool a role hands its work on with
func roleTool(role string) string {
	switch role {
	case RoleEngineer:
		return ToolReport
	case RoleLead:
		return ToolDirect
	}
	return ""
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

// Questions as the project manager puts them
type askInput struct {
	Questions []harness.Question `json:"questions"`
}

type askTool struct{ app *harness.App }

func (askTool) Definition() protocol.Tool {
	return protocol.Tool{Name: ToolAsk, Description: "Asks the person one to four multiple choice questions in their session prompt and waits for the answers. Each question offers two to four concrete options, and the person may always type an answer of their own instead. Ask only what you cannot settle from the request and the repository.", Parameters: json.RawMessage(`{"type":"object","properties":{
"questions":{"type":"array","minItems":1,"maxItems":4,"items":{"type":"object","properties":{
  "question":{"type":"string","description":"The question, in plain words"},
  "header":{"type":"string","description":"Short tag such as scope or storage, a few words at most"},
  "options":{"type":"array","minItems":2,"maxItems":4,"items":{"type":"object","properties":{
    "label":{"type":"string","description":"The option in a few words"},
    "description":{"type":"string","description":"What picking it means"}},
    "required":["label"],"additionalProperties":false}},
  "multi_select":{"type":"boolean","description":"The person may pick several options"}},
  "required":["question","options"],"additionalProperties":false}}},
"required":["questions"],"additionalProperties":false}`)}
}

func (t askTool) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in askInput
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	if err := checkQuestions(in.Questions); err != nil {
		return tool.Error(err.Error()), nil
	}
	id, err := sessionOf(t.app, ctx)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if s, _, ok := sprintOfSession(id); ok && !s.Snapshot().Running() {
		return tool.Error(unwaited(id, ToolAsk)), nil
	}
	if !Current().Interactive {
		return tool.Error("the sprint is not interactive, nobody answers questions; settle this yourself from the request and the repository and note the choice"), nil
	}
	answers, err := t.app.Questions.Ask(ctx, harness.RunFrom(ctx), id, in.Questions)
	if err != nil {
		return tool.Error("the questions were not answered: " + err.Error()), nil
	}
	return tool.Text(answersText(in.Questions, answers)).With(answers), nil
}

// Refuses no questions, too many, one without text, or options too few, too many, blank or repeated
func checkQuestions(qs []harness.Question) error {
	if len(qs) == 0 {
		return errors.New("at least one question is required")
	}
	if len(qs) > maxQuestions {
		return fmt.Errorf("%d questions is more than the %d one call may ask", len(qs), maxQuestions)
	}
	for i := range qs {
		q := &qs[i]
		q.Text = strings.TrimSpace(q.Text)
		if q.Text == "" {
			return fmt.Errorf("question %d has no text", i+1)
		}
		if len(q.Choices) < 2 || len(q.Choices) > maxChoices {
			return fmt.Errorf("question %d needs two to %d options, it has %d", i+1, maxChoices, len(q.Choices))
		}
		seen := map[string]bool{}
		for j := range q.Choices {
			label := strings.TrimSpace(q.Choices[j].Label)
			q.Choices[j].Label = label
			if label == "" {
				return fmt.Errorf("question %d option %d has no label", i+1, j+1)
			}
			if seen[strings.ToLower(label)] {
				return fmt.Errorf("question %d offers %q twice", i+1, label)
			}
			seen[strings.ToLower(label)] = true
		}
	}
	return nil
}

// Report an engineer makes on its ticket
type reportInput struct {
	Ticket string `json:"ticket"`
	Status string `json:"status"`
	Report string `json:"report"`
}

type reportTool struct{ app *harness.App }

func (reportTool) Definition() protocol.Tool {
	return protocol.Tool{Name: ToolReport, Description: "Reports to the lead on your ticket and waits for direction. Report done when the ticket is built and verified, blocked when you cannot go on, question when a choice is not yours to make. The answer is the direction of the lead: follow it, and end your turn once the lead accepts or fails the ticket.", Parameters: json.RawMessage(`{"type":"object","properties":{
"ticket":{"type":"string","description":"Id of your ticket"},
"status":{"type":"string","enum":["done","blocked","question"]},
"report":{"type":"string","description":"For done, what changed file by file and how it was verified; for blocked or question, exactly what stands in the way or needs deciding"}},
"required":["ticket","status","report"],"additionalProperties":false}`)}
}

func (t reportTool) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in reportInput
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	in.Ticket, in.Status, in.Report = strings.TrimSpace(in.Ticket), strings.TrimSpace(in.Status), strings.TrimSpace(in.Report)
	if !contains(reportStatuses, in.Status) {
		return tool.Errorf("status must be one of %s, got %q", strings.Join(reportStatuses, ", "), in.Status), nil
	}
	if in.Report == "" {
		return tool.Error("report is required"), nil
	}
	s, m, err := memberOf(t.app, ctx, RoleEngineer)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	r := &report{Ticket: in.Ticket, Engineer: m.Name, Status: in.Status, Text: in.Report, Filed: time.Now(), reply: make(chan direction, 1)}
	if err := s.file(ctx, r); err != nil {
		return tool.Error(err.Error()), nil
	}
	select {
	case d := <-r.reply:
		return tool.Text(directionText(in.Ticket, d)).With(d), nil
	case <-ctx.Done():
		s.withdraw(r)
		return tool.Error("the run ended before the lead directed you: " + ctx.Err().Error()), nil
	}
}

// Direction the lead gives on a report
type directInput struct {
	Ticket  string `json:"ticket"`
	Verdict string `json:"verdict"`
	Notes   string `json:"notes"`
}

type directTool struct{ app *harness.App }

func (directTool) Definition() protocol.Tool {
	return protocol.Tool{Name: ToolDirect, Description: "Answers the report an engineer is waiting on. Accepted closes the ticket as done, rework sends it back with what must change, guidance answers a blocked or questioning engineer who then goes on, failed closes the ticket as not done.", Parameters: json.RawMessage(`{"type":"object","properties":{
"ticket":{"type":"string","description":"Id of the ticket reported on"},
"verdict":{"type":"string","enum":["accepted","rework","guidance","failed"]},
"notes":{"type":"string","description":"For rework, exactly what is wrong and what done looks like; for guidance, the decision or the way past the block; for failed, why"}},
"required":["ticket","verdict"],"additionalProperties":false}`)}
}

func (t directTool) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in directInput
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	in.Ticket, in.Verdict, in.Notes = strings.TrimSpace(in.Ticket), strings.TrimSpace(in.Verdict), strings.TrimSpace(in.Notes)
	if !contains(verdicts, in.Verdict) {
		return tool.Errorf("verdict must be one of %s, got %q", strings.Join(verdicts, ", "), in.Verdict), nil
	}
	if in.Verdict != VerdictAccepted && in.Notes == "" {
		return tool.Errorf("%s needs notes", in.Verdict), nil
	}
	s, _, err := memberOf(t.app, ctx, RoleLead)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	d := direction{Verdict: in.Verdict, Notes: in.Notes}
	r, err := s.direct(in.Ticket, d, Current().Team.MaxRework)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	left := s.openReports()
	text := fmt.Sprintf("%s told %s on ticket %s.", r.Engineer, in.Verdict, in.Ticket)
	if len(left) > 0 {
		text += fmt.Sprintf(" %d reports still wait for you: %s.", len(left), strings.Join(reportIDs(left), ", "))
	} else {
		text += " No report waits for you now, end your turn."
	}
	return tool.Text(text).With(d), nil
}

// Ticket ids of the reports, in filing order
func reportIDs(reports []*report) []string {
	out := make([]string, 0, len(reports))
	for _, r := range reports {
		out = append(out, r.Ticket)
	}
	return out
}

// Ticket the review sends back with what must change
type sendBack struct {
	Ticket string `json:"ticket"`
	Notes  string `json:"notes"`
}

// Review as the lead submits it
type reviewInput struct {
	Summary string     `json:"summary"`
	Rework  []sendBack `json:"rework,omitempty"`
}

type reviewTool struct{ app *harness.App }

func (reviewTool) Definition() protocol.Tool {
	return protocol.Tool{Name: ToolReview, Description: "Closes the review of a build round with a summary of the work as a whole and the tickets to send back, if any. A ticket sent back goes to its engineer with the notes for another round.", Parameters: json.RawMessage(`{"type":"object","properties":{
"summary":{"type":"string","description":"State of the work as a whole, what was built, how it was verified and what the person should know"},
"rework":{"type":"array","items":{"type":"object","properties":{
  "ticket":{"type":"string","description":"Ticket id"},
  "notes":{"type":"string","description":"Exactly what is wrong and what done looks like"}},
  "required":["ticket","notes"],"additionalProperties":false}}},
"required":["summary"],"additionalProperties":false}`)}
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
	s, _, err := memberOf(t.app, ctx, RoleLead)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if err := checkReview(in, s.Snapshot(), Current().Team.MaxRework); err != nil {
		return tool.Error(err.Error()), nil
	}
	id, err := sessionOf(t.app, ctx)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if err := deliver(id, ToolReview, in); err != nil {
		return tool.Error(err.Error()), nil
	}
	return tool.Text(fmt.Sprintf("Review recorded, %d sent back.\n\n", len(in.Rework)) + reviewText(in)).With(in), nil
}

// Refuses a review sending back an unknown ticket, one twice, one without notes or one past the rework allowed
func checkReview(in reviewInput, snap Snapshot, maxRework int) error {
	seen := map[string]bool{}
	for _, back := range in.Rework {
		t, ok := snap.Ticket(back.Ticket)
		if !ok {
			return fmt.Errorf("no ticket %s to send back", back.Ticket)
		}
		if seen[back.Ticket] {
			return fmt.Errorf("ticket %s is sent back twice", back.Ticket)
		}
		seen[back.Ticket] = true
		if strings.TrimSpace(back.Notes) == "" {
			return fmt.Errorf("ticket %s: sending it back needs notes", back.Ticket)
		}
		if t.Round >= maxRework {
			return fmt.Errorf("ticket %s was sent back %d times, the most the sprint allows, leave it and say so in the summary", back.Ticket, t.Round)
		}
	}
	return nil
}
