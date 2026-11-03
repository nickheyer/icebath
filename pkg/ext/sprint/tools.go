package sprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

// Tools the roles hand their work on with
const (
	ToolBrief   = "SprintBrief"
	ToolAsk     = "SprintAsk"
	ToolPlan    = "SprintPlan"
	ToolHandoff = "SprintHandoff"
	ToolVerdict = "SprintVerdict"
	ToolMessage = "SprintMessage"
)

// Most questions one ask puts and most choices one question offers
const (
	maxQuestions = 4
	maxChoices   = 4
)

// Tools of the sprint built once the app stands
func sprintTools(app *harness.App) []tool.Tool {
	return []tool.Tool{briefTool{app}, askTool{app}, planTool{app}, handoffTool{app}, verdictTool{app}, messageTool{app}}
}

// Sprint and member of the session the calling run serves
func callerOf(app *harness.App, ctx context.Context) (*Sprint, *Member, error) {
	sess := app.SessionFor(ctx)
	if sess == nil {
		return nil, nil, errors.New("this tool is for members of a sprint team, start one with /" + Name + " <request>")
	}
	s, m, ok := sprintOfSession(sess.ID())
	if !ok {
		return nil, nil, errors.New("this session is not part of a sprint, start one with /" + Name + " <request>")
	}
	if snap := s.Snapshot(); !snap.Running() {
		return nil, nil, fmt.Errorf("%s ended with %s, start another with /%s <request>", s.ID, snap.Status, Name)
	}
	return s, m, nil
}

// Refuses a call from a member of another role
func mustBe(m *Member, role Role, toolName string) error {
	if m.Role != role {
		return fmt.Errorf("%s is for the %s, you are the %s", toolName, role, m.Role)
	}
	return nil
}

type briefTool struct{ app *harness.App }

func (briefTool) Definition() protocol.Tool {
	return protocol.Tool{Name: ToolBrief, Description: "Hands the settled brief to the sprint team, ending the brief stage. Call it once, when the objective is clear enough to plan from.", Parameters: json.RawMessage(`{"type":"object","properties":{
"objective":{"type":"string","description":"What the sprint delivers, in a paragraph the architect can plan from"},
"acceptance":{"type":"array","items":{"type":"string"},"description":"Checks that show the objective is met"},
"constraints":{"type":"array","items":{"type":"string"},"description":"Rules the work must keep to"},
"untouched":{"type":"array","items":{"type":"string"},"description":"Things the team must leave alone"},
"notes":{"type":"string","description":"Choices settled with the person or on their behalf"}},
"required":["objective"],"additionalProperties":false}`)}
}

func (briefTool) Annotations() tool.Annotations { return tool.Annotations{Idempotent: true} }

func (t briefTool) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in Brief
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	if strings.TrimSpace(in.Objective) == "" {
		return tool.Error("objective is required"), nil
	}
	s, m, err := callerOf(t.app, ctx)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if err := mustBe(m, RoleDirector, ToolBrief); err != nil {
		return tool.Error(err.Error()), nil
	}
	if err := s.deliver(m.SessionID(), ToolBrief, in); err != nil {
		return tool.Error(err.Error()), nil
	}
	return tool.Text(briefTaken(in)).With(in), nil
}

// Questions as the director puts them
type askInput struct {
	Questions []harness.Question `json:"questions"`
}

type askTool struct{ app *harness.App }

func (askTool) Definition() protocol.Tool {
	return protocol.Tool{Name: ToolAsk, Description: "Asks the person one to four multiple choice questions and waits for the answers. Each question offers two to four concrete options, and the person may type an answer of their own instead. Ask only what the request and the repository cannot settle.", Parameters: json.RawMessage(`{"type":"object","properties":{
"questions":{"type":"array","minItems":1,"maxItems":4,"items":{"type":"object","properties":{
  "question":{"type":"string","description":"The question, in plain words"},
  "header":{"type":"string","description":"Short tag such as scope or storage"},
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
	s, m, err := callerOf(t.app, ctx)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if err := mustBe(m, RoleDirector, ToolAsk); err != nil {
		return tool.Error(err.Error()), nil
	}
	var asked []string
	for _, q := range in.Questions {
		asked = append(asked, q.Text)
	}
	s.say(MsgQuestion, m.Name, Person, "", strings.Join(asked, " "))
	s.wait(m.Name)
	defer s.wait("")
	answers, err := t.app.Questions.Ask(ctx, harness.RunFrom(ctx), m.SessionID(), in.Questions)
	if err != nil {
		return tool.Error("the questions went unanswered: " + err.Error()), nil
	}
	text := answersText(in.Questions, answers)
	s.say(MsgAnswer, Person, m.Name, "", text)
	return tool.Text(text).With(answers), nil
}

// Trims the questions and refuses an empty, oversized or lopsided set
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
			return fmt.Errorf("question %d must offer two to %d options", i+1, maxChoices)
		}
		seen := map[string]bool{}
		for j := range q.Choices {
			c := &q.Choices[j]
			c.Label = strings.TrimSpace(c.Label)
			if c.Label == "" {
				return fmt.Errorf("question %d option %d has no label", i+1, j+1)
			}
			if key := strings.ToLower(c.Label); seen[key] {
				return fmt.Errorf("question %d offers %q twice", i+1, c.Label)
			} else {
				seen[key] = true
			}
		}
	}
	return nil
}

// Plan as the architect submits it
type planInput struct {
	Plan
	Tickets []Ticket `json:"tickets"`
}

type planTool struct{ app *harness.App }

func (planTool) Definition() protocol.Tool {
	return protocol.Tool{Name: ToolPlan, Description: "Hands the plan and its tickets to the sprint team, ending the plan stage. Call it once with every ticket.", Parameters: json.RawMessage(`{"type":"object","properties":{
"title":{"type":"string","description":"The change in a few words"},
"summary":{"type":"string","description":"The change as a whole"},
"approach":{"type":"string","description":"How the pieces fit and what order they go in"},
"risks":{"type":"array","items":{"type":"string"},"description":"What could go wrong"},
"tickets":{"type":"array","items":{"type":"object","properties":{
  "id":{"type":"string","description":"Short id such as T1, given in order when omitted"},
  "title":{"type":"string"},
  "description":{"type":"string","description":"What the builder delivers, the builder also has the brief"},
  "files":{"type":"array","items":{"type":"string"},"description":"Files the ticket owns, which no other ticket edits"},
  "depends_on":{"type":"array","items":{"type":"string"},"description":"Ids of tickets that must be done first"},
  "size":{"type":"string","description":"Rough size such as small or large"}},
  "required":["title","description"],"additionalProperties":false}}},
"required":["title","summary","tickets"],"additionalProperties":false}`)}
}

func (planTool) Annotations() tool.Annotations { return tool.Annotations{Idempotent: true} }

func (t planTool) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in planInput
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	cfg, err := Load(t.app.Config)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if err := checkPlan(&in, cfg.Team.MaxTickets); err != nil {
		return tool.Error(err.Error()), nil
	}
	s, m, err := callerOf(t.app, ctx)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if err := mustBe(m, RoleArchitect, ToolPlan); err != nil {
		return tool.Error(err.Error()), nil
	}
	if err := s.deliver(m.SessionID(), ToolPlan, in); err != nil {
		return tool.Error(err.Error()), nil
	}
	return tool.Text(planTaken(in)).With(in), nil
}

// Fills in missing ids, then refuses an empty plan, too many tickets, duplicate ids, unknown dependencies or a cycle
func checkPlan(in *planInput, maxTickets int) error {
	if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Summary) == "" {
		return errors.New("the plan needs a title and a summary")
	}
	if len(in.Tickets) == 0 {
		return errors.New("at least one ticket is required")
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

type handoffTool struct{ app *harness.App }

func (handoffTool) Definition() protocol.Tool {
	return protocol.Tool{Name: ToolHandoff, Description: "Hands your ticket to the reviewer and waits for the verdict. Call it when the ticket is done, when you are blocked, or with a question only the reviewer can settle.", Parameters: json.RawMessage(`{"type":"object","properties":{
"ticket":{"type":"string","description":"Id of your ticket"},
"status":{"type":"string","enum":["done","blocked","question"]},
"summary":{"type":"string","description":"What you built and how, what blocks you, or the question"},
"files_changed":{"type":"array","items":{"type":"string"}},
"tested":{"type":"string","description":"How you checked the work, the commands run and what they showed"}},
"required":["ticket","status","summary"],"additionalProperties":false}`)}
}

func (t handoffTool) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in Handoff
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	in.Ticket, in.Summary = strings.TrimSpace(in.Ticket), strings.TrimSpace(in.Summary)
	if in.Ticket == "" || in.Summary == "" {
		return tool.Error("ticket and summary are required"), nil
	}
	if !slices.Contains(handoffStatuses, in.Status) {
		return tool.Error("status must be done, blocked or question"), nil
	}
	s, m, err := callerOf(t.app, ctx)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if err := mustBe(m, RoleBuilder, ToolHandoff); err != nil {
		return tool.Error(err.Error()), nil
	}
	if err := s.deliver(m.SessionID(), ToolHandoff, in); err != nil {
		return tool.Error(err.Error()), nil
	}
	return tool.Text(handoffTaken()).With(in), nil
}

type verdictTool struct{ app *harness.App }

func (verdictTool) Definition() protocol.Tool {
	return protocol.Tool{Name: ToolVerdict, Description: "Gives the verdict on a handoff. Accept a done ticket that meets its description and the brief, send it back for rework with precise notes, give guidance on a question or a block, or fail it when it cannot be built as planned.", Parameters: json.RawMessage(`{"type":"object","properties":{
"ticket":{"type":"string"},
"verdict":{"type":"string","enum":["accepted","rework","guidance","failed"]},
"notes":{"type":"string","description":"What to change, the answer, or why it failed"}},
"required":["ticket","verdict"],"additionalProperties":false}`)}
}

func (t verdictTool) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in Decision
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	in.Ticket, in.Notes = strings.TrimSpace(in.Ticket), strings.TrimSpace(in.Notes)
	if in.Ticket == "" {
		return tool.Error("ticket is required"), nil
	}
	if !slices.Contains(verdicts, in.Verdict) {
		return tool.Error("verdict must be accepted, rework, guidance or failed"), nil
	}
	if in.Verdict != VerdictAccepted && in.Notes == "" {
		return tool.Error("notes are required for " + string(in.Verdict)), nil
	}
	s, m, err := callerOf(t.app, ctx)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if err := mustBe(m, RoleReviewer, ToolVerdict); err != nil {
		return tool.Error(err.Error()), nil
	}
	if err := s.deliver(m.SessionID(), ToolVerdict, in); err != nil {
		return tool.Error(err.Error()), nil
	}
	return tool.Text(verdictTaken(in)).With(in), nil
}

// Address a message goes to for every other member of the team
const toTeam = "team"

// Message as a member sends it
type messageInput struct {
	To   string `json:"to"`
	Text string `json:"text"`
}

type messageTool struct{ app *harness.App }

func (messageTool) Definition() protocol.Tool {
	return protocol.Tool{Name: ToolMessage, Description: "Sends a short message to a member of your sprint team by name, to team for every other member, or to you for the person watching. It returns at once, the answer arrives as a new message, and it never ends your stage.", Parameters: json.RawMessage(`{"type":"object","properties":{
"to":{"type":"string","description":"Name of a member, team for every other member, or you for the person watching"},
"text":{"type":"string","description":"The message, short and concrete, with the exact change when you ask for one"}},
"required":["to","text"],"additionalProperties":false}`)}
}

func (t messageTool) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in messageInput
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	in.To, in.Text = strings.TrimSpace(in.To), strings.TrimSpace(in.Text)
	if in.Text == "" {
		return tool.Error("text is required"), nil
	}
	if in.To == "" {
		return tool.Error("to is required, a member name, " + toTeam + " or " + Person), nil
	}
	cfg, err := Load(t.app.Config)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	s, m, err := callerOf(t.app, ctx)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	names, person, err := address(s.roster(cfg), m.Name, in.To)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if person {
		s.say(MsgChat, m.Name, Person, s.ticketOf(m), in.Text)
		t.app.Announce(harness.NoticeMsg{Level: harness.Info, Text: m.Name + ": " + in.Text, Flow: s.ID})
		return tool.Text(personSent()).With(in), nil
	}
	type target struct {
		m    *Member
		live *harness.Live
	}
	var targets []target
	var missed []string
	for _, name := range names {
		mem := s.member(name)
		if mem == nil {
			missed = append(missed, name+" has not joined yet")
			continue
		}
		live, err := s.rejoin(ctx, t.app, mem)
		switch {
		case err != nil:
			missed = append(missed, name+" could not be reached, "+err.Error())
		case live == nil:
			missed = append(missed, name+" has not joined yet")
		default:
			targets = append(targets, target{mem, live})
		}
	}
	if len(targets) == 0 {
		return tool.Error(strings.Join(missed, ", ")), nil
	}
	label := toTeam
	if !strings.EqualFold(in.To, toTeam) {
		label = names[0]
	}
	s.say(MsgChat, m.Name, label, s.ticketOf(m), in.Text)
	text := chatText(*m, s.whereOf(m), label == toTeam, in.Text)
	var reached []string
	for _, tg := range targets {
		if err := s.post(t.app, m, tg.m, tg.live, text); err != nil {
			missed = append(missed, err.Error())
			continue
		}
		reached = append(reached, tg.m.Name)
	}
	if len(reached) == 0 {
		s.say(MsgFailure, label, m.Name, s.ticketOf(m), "message from "+m.Name+" reached nobody, "+strings.Join(missed, ", "))
		return tool.Error(strings.Join(missed, ", ")), nil
	}
	return tool.Text(chatSent(reached, missed)).With(in), nil
}

// Names the sprint staffs, its members in the order they joined, then the roles still to join
func (s *Sprint) roster(cfg Config) []string {
	snap := s.Snapshot()
	var names []string
	add := func(name string) {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	for _, m := range snap.Members {
		add(m.Name)
	}
	for _, role := range Roles {
		if role != RoleBuilder {
			add(cfg.NameOf(role, 0))
			continue
		}
		for i := range builderCount(cfg.Team, len(snap.Tickets)) {
			add(cfg.NameOf(role, i))
		}
	}
	return names
}

// Members a message goes to by name, every other one for team, or the person, refusing the sender and strangers
func address(roster []string, from, to string) ([]string, bool, error) {
	switch {
	case strings.EqualFold(to, Person):
		return nil, true, nil
	case strings.EqualFold(to, toTeam):
		var others []string
		for _, name := range roster {
			if name != from {
				others = append(others, name)
			}
		}
		if len(others) == 0 {
			return nil, false, errors.New("nobody else is on the team")
		}
		return others, false, nil
	}
	for _, name := range roster {
		if !strings.EqualFold(name, to) {
			continue
		}
		if name == from {
			return nil, false, fmt.Errorf("%s is you, send to another member, %s or %s", name, toTeam, Person)
		}
		return []string{name}, false, nil
	}
	return nil, false, fmt.Errorf("no member is named %q, send to one of %s, or to %s or %s", to, strings.Join(roster, ", "), toTeam, Person)
}

// Ticket a member holds with its title, or the stage the sprint is in while it holds none
func (s *Sprint) whereOf(m *Member) string {
	snap := s.Snapshot()
	id := s.ticketOf(m)
	if id == "" {
		return string(snap.Stage) + " stage"
	}
	if t, ok := snap.Ticket(id); ok {
		return t.ID + " " + t.Title
	}
	return id
}
