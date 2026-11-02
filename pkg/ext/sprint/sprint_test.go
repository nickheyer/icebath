package sprint

import (
	"context"
	"encoding/json"
	"errors"
	"image/color"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
)

func TestConfigLoad(t *testing.T) {
	cfg := harness.Config{Settings: map[string]string{
		Name + ".director.profile": "anthropic", Name + ".director.model": "claude-fable-5-1", Name + ".builder.max_tokens": "4096", Name + ".builder.budget": "500000",
		Name + ".team.builders": "5", Name + ".team.parallel": "false", Name + ".limits.build": "45m", Name + ".interactive": "false",
		Name + ".arena.pixels": "half", Name + ".arena.fps": "20", Name + ".look.builder": "teal cap, rose crown", Name + ".name.builder": "Ann, Bob", Name + ".look.director": "gold fedora",
	}}
	c, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c.Roles[RoleDirector].Profile != "anthropic" || c.Roles[RoleBuilder].MaxTokens != 4096 || c.Roles[RoleBuilder].Budget != 500000 {
		t.Fatalf("roles %+v", c.Roles)
	}
	if c.Team.Builders != 5 || c.Team.Parallel || c.Limits.Build != 45*time.Minute || c.Interactive {
		t.Fatalf("team %+v limits %+v interactive %v", c.Team, c.Limits, c.Interactive)
	}
	if c.Arena.Pixels != "half" || c.Arena.FPS != 20 {
		t.Fatalf("arena %+v", c.Arena)
	}
	if c.NameOf(RoleBuilder, 1) != "Bob" || c.NameOf(RoleBuilder, 2) != defaultBuilderNames[2] || c.NameOf(RoleDirector, 0) != "Director" {
		t.Fatalf("names %q %q %q", c.NameOf(RoleBuilder, 1), c.NameOf(RoleBuilder, 2), c.NameOf(RoleDirector, 0))
	}
	if c.LookOf(RoleBuilder, 1) != (Look{"rose", "crown"}) || c.LookOf(RoleBuilder, 4) != defaultBuilderLooks[4] || c.LookOf(RoleDirector, 0) != (Look{"gold", "fedora"}) {
		t.Fatalf("looks %+v", c.Looks)
	}
	for _, bad := range []map[string]string{
		{Name + ".team.builders": "0"}, {Name + ".team.builders": "nine"}, {Name + ".limits.plan": "0s"}, {Name + ".interactive": "maybe"},
		{Name + ".arena.pixels": "braille"}, {Name + ".arena.fps": "2"}, {Name + ".look.builder": "teal"}, {Name + ".look.reviewer": "plaid cap"},
	} {
		if _, err := Load(harness.Config{Settings: bad}); err == nil {
			t.Fatalf("settings %v loaded", bad)
		}
	}
	for _, s := range Settings() {
		if s.Check == nil {
			continue
		}
		if err := s.Validate(""); err == nil && (strings.HasSuffix(s.Key, "limits.total") || strings.HasSuffix(s.Key, ".name.builder")) {
			t.Fatalf("%s took an empty value", s.Key)
		}
	}
	if err := Settings()[0].Validate("not a name!"); err == nil {
		t.Fatal("profile setting took an invalid name")
	}
	values := Values(cfg)
	if values[Name+".team.builders"] != "5" || len(values) != len(Settings()) {
		t.Fatalf("values %v", values)
	}
}

func TestKinds(t *testing.T) {
	c := Default()
	c.Roles[RoleReviewer] = RoleConfig{Profile: "openai", Model: "gpt-5", MaxTokens: 2048, MaxRounds: 30}
	kinds := c.Kinds()
	if len(kinds) != len(Roles) {
		t.Fatalf("%d kinds", len(kinds))
	}
	for _, k := range kinds {
		if err := k.Validate(); err != nil {
			t.Fatalf("%s: %v", k.Name, err)
		}
	}
	reviewer := c.Kind(RoleReviewer)
	if reviewer.Model == nil || reviewer.Model.Profile != "openai" || reviewer.Model.Model != "gpt-5" || reviewer.MaxTokens != 2048 || reviewer.MaxRounds != 30 {
		t.Fatalf("reviewer kind %+v", reviewer)
	}
	if !reviewer.Offers("Shell") || !reviewer.Offers(ToolVerdict) || reviewer.Offers("Edit") || reviewer.Offers(ToolHandoff) || reviewer.Offers("Agent") {
		t.Fatalf("reviewer tools %+v", reviewer)
	}
	director := c.Kind(RoleDirector)
	if !director.Offers(ToolBrief) || !director.Offers(ToolAsk) || director.Offers("Write") || director.Offers(ToolPlan) {
		t.Fatalf("director tools %+v", director)
	}
	architect := c.Kind(RoleArchitect)
	if !architect.Offers(ToolPlan) || architect.Offers(ToolAsk) || architect.Offers("Edit") || architect.Offers("Shell") {
		t.Fatalf("architect tools %+v", architect)
	}
	builder := c.Kind(RoleBuilder)
	if !builder.Offers("Edit") || !builder.Offers("mcp__x__y") || !builder.Offers(ToolHandoff) || builder.Offers("Agent") || builder.Offers(ToolVerdict) || builder.Offers(ToolBrief) {
		t.Fatalf("builder tools %+v", builder)
	}
	if role, ok := RoleOfKind(builder.Name); !ok || role != RoleBuilder {
		t.Fatalf("role of %s = %q %v", builder.Name, role, ok)
	}
	if _, ok := RoleOfKind("review"); ok {
		t.Fatal("foreign kind taken as a role")
	}
}

func TestCheckPlan(t *testing.T) {
	in := planInput{Plan: Plan{Title: "t", Summary: "s"}, Tickets: []Ticket{{Title: "a", Description: "d"}, {ID: "x", Title: "b", Description: "d", DependsOn: []string{"T1"}}}}
	if err := checkPlan(&in, 5); err != nil {
		t.Fatal(err)
	}
	if in.Tickets[0].ID != "T1" || in.Tickets[1].ID != "x" || in.Tickets[0].Status != TicketTodo {
		t.Fatalf("ids %+v", in.Tickets)
	}
	bad := []planInput{
		{Plan: Plan{Title: "t", Summary: "s"}},
		{Plan: Plan{Title: "t"}, Tickets: []Ticket{{Title: "a", Description: "d"}}},
		{Plan: Plan{Title: "t", Summary: "s"}, Tickets: []Ticket{{ID: "a", Title: "a", Description: "d"}, {ID: "a", Title: "b", Description: "d"}}},
		{Plan: Plan{Title: "t", Summary: "s"}, Tickets: []Ticket{{ID: "a", Title: "a", Description: "d", DependsOn: []string{"zz"}}}},
		{Plan: Plan{Title: "t", Summary: "s"}, Tickets: []Ticket{{ID: "a", Title: "a", Description: "d", DependsOn: []string{"b"}}, {ID: "b", Title: "b", Description: "d", DependsOn: []string{"a"}}}},
		{Plan: Plan{Title: "t", Summary: "s"}, Tickets: []Ticket{{Title: "a", Description: "d"}, {Title: "b", Description: "d"}}},
	}
	for i := range bad {
		limit := 5
		if i == len(bad)-1 {
			limit = 1
		}
		if err := checkPlan(&bad[i], limit); err == nil {
			t.Fatalf("plan %d passed", i)
		}
	}
}

func TestCheckQuestions(t *testing.T) {
	good := []harness.Question{{Text: " Which store? ", Choices: []harness.Choice{{Label: " Postgres "}, {Label: "SQLite"}}}}
	if err := checkQuestions(good); err != nil {
		t.Fatal(err)
	}
	if good[0].Text != "Which store?" || good[0].Choices[0].Label != "Postgres" {
		t.Fatalf("not trimmed: %+v", good[0])
	}
	two := []harness.Choice{{Label: "a"}, {Label: "b"}}
	bad := [][]harness.Question{
		nil,
		{{Text: "q", Choices: two}, {Text: "q", Choices: two}, {Text: "q", Choices: two}, {Text: "q", Choices: two}, {Text: "q", Choices: two}},
		{{Text: " ", Choices: two}},
		{{Text: "q", Choices: two[:1]}},
		{{Text: "q", Choices: []harness.Choice{{Label: "a"}, {Label: "b"}, {Label: "c"}, {Label: "d"}, {Label: "e"}}}},
		{{Text: "q", Choices: []harness.Choice{{Label: "a"}, {Label: " "}}}},
		{{Text: "q", Choices: []harness.Choice{{Label: "a"}, {Label: "A"}}}},
	}
	for i, qs := range bad {
		if err := checkQuestions(qs); err == nil {
			t.Fatalf("questions %d passed", i)
		}
	}
	text := answersText(good, []harness.Answer{{Picked: []string{"Postgres"}, Text: "with pgx"}})
	for _, want := range []string{"Which store?", "Picked: Postgres", "In their own words: with pgx"} {
		if !strings.Contains(text, want) {
			t.Fatalf("answers text lacks %q: %s", want, text)
		}
	}
	if text := answersText(good, []harness.Answer{{Declined: true}}); !strings.Contains(text, "No answer") {
		t.Fatalf("declined answer: %s", text)
	}
}

// Sprint in its build with tickets in every state and a builder on one, as the scheduler finds it
func building(t *testing.T) (*Sprint, *Ticket) {
	t.Helper()
	s := newSprint("sprint-7", "do it", time.Now(), nil)
	s.enter(StageBuild, "building")
	tk := &Ticket{ID: "T1", Title: "one", Description: "d", Status: TicketActive, Builder: "Ada"}
	s.setPlan(Plan{Title: "p", Summary: "x"}, []*Ticket{tk, {ID: "T2", Title: "two", Description: "d", Status: TicketTodo, DependsOn: []string{"T1"}}, {ID: "T3", Title: "three", Description: "d", Status: TicketTodo}})
	return s, tk
}

func TestReadyAndBlocked(t *testing.T) {
	s, tk := building(t)
	ready := s.ready()
	if len(ready) != 1 || ready[0].ID != "T3" {
		t.Fatalf("ready %v", ready)
	}
	s.update(tk, func(t *Ticket) { t.Status = TicketDone })
	ready = s.ready()
	if len(ready) != 2 || ready[0].ID != "T2" {
		t.Fatalf("ready after done %v", ready)
	}
	s.update(tk, func(t *Ticket) { t.Status = TicketFailed })
	if n := s.blockDependents(); n != 1 || s.ticket("T2").Status != TicketBlocked {
		t.Fatalf("blocked %d, T2 is %s", n, s.ticket("T2").Status)
	}
	if s.settled() {
		t.Fatal("settled with T3 to do")
	}
	if n := s.strand(); n != 1 || !s.settled() {
		t.Fatalf("stranded %d settled %v", n, s.settled())
	}
	snap := s.Snapshot()
	if done, total := snap.Progress(); done != 0 || total != 3 {
		t.Fatalf("progress %d/%d", done, total)
	}
}

func TestDeliverChecks(t *testing.T) {
	s, tk := building(t)
	w := s.await("sess", ToolHandoff, func(v any) error {
		if v.(Handoff).Ticket != tk.ID {
			return errors.New("not yours")
		}
		return nil
	})
	defer s.release("sess", w)
	if err := s.deliver("sess", ToolVerdict, Decision{}); err == nil || !strings.Contains(err.Error(), "waits for "+ToolHandoff) {
		t.Fatalf("wrong tool: %v", err)
	}
	if err := s.deliver("other", ToolHandoff, Handoff{}); err == nil {
		t.Fatal("wrong session delivered")
	}
	if err := s.deliver("sess", ToolHandoff, Handoff{Ticket: "T2"}); err == nil || err.Error() != "not yours" {
		t.Fatalf("check: %v", err)
	}
	if err := s.deliver("sess", ToolHandoff, Handoff{Ticket: "T1", Status: HandoffDone}); err != nil {
		t.Fatal(err)
	}
	if err := s.deliver("sess", ToolHandoff, Handoff{Ticket: "T1"}); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("second delivery: %v", err)
	}
	if v := <-w.ch; v.(Handoff).Status != HandoffDone {
		t.Fatalf("got %+v", v)
	}
	s.release("sess", w)
	if err := s.deliver("sess", ToolHandoff, Handoff{}); err == nil {
		t.Fatal("delivered after release")
	}
}

func TestPauseResumeWakesEveryWaiter(t *testing.T) {
	s, _ := building(t)
	if s.Resume() {
		t.Fatal("resumed while not paused")
	}
	if !s.Pause() || s.Pause() {
		t.Fatal("pause did not take once")
	}
	woke := make(chan struct{}, 3)
	for range 3 {
		ch := s.resumed()
		go func() { <-ch; woke <- struct{}{} }()
	}
	if !s.Resume() {
		t.Fatal("resume refused")
	}
	for range 3 {
		select {
		case <-woke:
		case <-time.After(2 * time.Second):
			t.Fatal("a waiter never woke")
		}
	}
	if s.Snapshot().Paused != "" {
		t.Fatal("still paused")
	}
	s.pause("stalled")
	s.wait("Ada")
	if snap := s.Snapshot(); snap.Paused == "" || snap.Waiting != "Ada" {
		t.Fatalf("stall %+v", snap)
	}
	s.finish(errors.New("boom"))
	if s.Resume() {
		t.Fatal("resumed an ended sprint")
	}
}

func TestRestartResetsTheSprint(t *testing.T) {
	s, tk := building(t)
	s.setBrief(Brief{Objective: "x"})
	live := &harness.Live{Session: session.New()}
	s.putMember(&Member{Role: RoleReviewer, Name: "Reviewer", Live: live})
	s.putMember(&Member{Role: RoleReviewer, Name: "Reviewer", Live: &harness.Live{Session: session.New()}})
	if n := len(s.Snapshot().Members); n != 1 {
		t.Fatalf("%d members after putting the same name twice", n)
	}
	s.update(tk, func(t *Ticket) { t.Status = TicketReview })
	t3 := s.ticket("T3")
	s.update(t3, func(t *Ticket) { t.Status, t.Err = TicketFailed, "failed by the reviewer: hopeless" })
	s.setPlan(s.Plan, append(s.Tickets, &Ticket{ID: "T4", Title: "four", Description: "d", Status: TicketFailed, Err: "Ada on T4: stalled"}, &Ticket{ID: "T5", Title: "five", Description: "d", Status: TicketDone}))
	s.finish(context.Canceled)
	snap := s.Snapshot()
	if snap.Running() || !snap.Stopped() || !snap.Restartable() || snap.Last != StageBuild || restartStage(snap) != StageBuild {
		t.Fatalf("stopped snapshot %+v", snap)
	}
	if from := s.restart(); from != StageBuild {
		t.Fatalf("restart from %s", from)
	}
	snap = s.Snapshot()
	if !snap.Running() || snap.Stage != StageBuild || snap.Paused != "" || snap.Err != nil {
		t.Fatalf("restarted snapshot %+v", snap)
	}
	want := map[string]TicketStatus{"T1": TicketTodo, "T2": TicketTodo, "T3": TicketFailed, "T4": TicketTodo, "T5": TicketDone}
	for _, tk := range snap.Tickets {
		if tk.Status != want[tk.ID] {
			t.Fatalf("ticket %s is %s after restart, want %s", tk.ID, tk.Status, want[tk.ID])
		}
	}
	s.finish(nil)
	if snap := s.Snapshot(); snap.Restartable() || snap.Last != StageDone || snap.Stage != StageDone {
		t.Fatalf("done snapshot %+v", snap)
	}
	bare := newSprint("sprint-8", "x", time.Now(), nil)
	bare.enter(StagePlan, "cutting")
	bare.finish(errors.New("boom"))
	if from := bare.restart(); from != StageBrief {
		t.Fatalf("a sprint with no brief restarts at %s", from)
	}
	planned := newSprint("sprint-9", "x", time.Now(), nil)
	planned.setBrief(Brief{Objective: "x"})
	planned.enter(StagePlan, "cutting")
	planned.finish(errors.New("boom"))
	if from := planned.restart(); from != StagePlan {
		t.Fatalf("a sprint with a brief and no plan restarts at %s", from)
	}
}

func TestMessagesAndRegistry(t *testing.T) {
	var got []any
	s := newSprint("sprint-10", "x", time.Now(), func(msg any) { got = append(got, msg) })
	m := s.say(MsgAssign, "Reviewer", "Ada", "T1", "take T1")
	s.say(MsgHandoff, "Ada", "Reviewer", "T1", "done")
	snap := s.Snapshot()
	if len(snap.Messages) != 2 || len(snap.MessagesAfter(m.Seq)) != 1 || snap.MessagesAfter(m.Seq)[0].Kind != MsgHandoff || len(snap.MessagesAfter(99)) != 0 {
		t.Fatalf("messages %+v", snap.Messages)
	}
	if len(got) == 0 {
		t.Fatal("no change announced")
	}
	if _, ok := got[0].(ChangedMsg); !ok {
		t.Fatalf("announced %T", got[0])
	}
	live := &harness.Live{Session: session.New()}
	s.putMember(&Member{Role: RoleBuilder, Name: "Ada", Live: live})
	register(s)
	t.Cleanup(func() {
		regMu.Lock()
		sprints = sprints[:len(sprints)-1]
		regMu.Unlock()
	})
	if found, mem, ok := sprintOfSession(live.Session.ID()); !ok || found != s || mem.Name != "Ada" {
		t.Fatalf("sprint of session: %v %+v %v", found, mem, ok)
	}
	if snap, ok := Lookup("sprint-10"); !ok || snap.ID != "sprint-10" {
		t.Fatalf("lookup %+v %v", snap, ok)
	}
	if _, _, ok := sprintOfSession("nobody"); ok {
		t.Fatal("found a sprint for a stranger")
	}
	s.spend("Ada", 1234)
	if s.Snapshot().Spent["Ada"] != 1234 {
		t.Fatal("spend not kept")
	}
}

func TestTransient(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, agent.ErrInterrupted, errBudget, errExpired, errStalled, agent.ErrRounds, &protocol.Error{Status: 401, Auth: true}, &protocol.Error{Status: 400}, nil} {
		if transient(err) {
			t.Fatalf("%v taken as transient", err)
		}
	}
	for _, err := range []error{errors.New("stream ended without a done event"), &protocol.Error{Status: 503, Transient: true}, errors.New("harness: a run is going on s1"), errors.New("connection reset"), errors.New("endpoint overloaded")} {
		if !transient(err) {
			t.Fatalf("%v not taken as transient", err)
		}
	}
}

func TestPrompts(t *testing.T) {
	b := Brief{Objective: "ship it", Acceptance: []string{"tests pass"}, Constraints: []string{"no new deps"}, Untouched: []string{"docs"}}
	p := Plan{Title: "Ship", Summary: "all of it", Approach: "piece by piece", Risks: []string{"time"}}
	tickets := []Ticket{{ID: "T1", Title: "one", Description: "do one", Files: []string{"a.go"}, Report: "did one", Status: TicketDone, Builder: "Ada"}, {ID: "T2", Title: "two", Description: "do two", DependsOn: []string{"T1"}, Notes: []string{"fix the name"}, Round: 1}}
	h := Handoff{Ticket: "T2", Status: HandoffDone, Summary: "built two", Files: []string{"b.go"}, Tested: "go test"}
	texts := map[string]string{
		"director":  directorOpening("make it", true),
		"architect": architectOpening(b, 12, 3),
		"builder":   builderOpening(b, p, tickets, tickets[1]),
		"next":      builderNext(tickets[1], tickets),
		"review":    reviewOpening(b, p, tickets[1], h, "Linus", true),
		"verdict":   verdictText(Decision{Ticket: "T2", Verdict: VerdictRework, Notes: "tests"}),
		"report":    reportOpening(b, p, tickets, errors.New("build stage: boom")),
		"plan":      planText(p, tickets),
		"wrap":      wrapUp(ToolPlan, 30*time.Minute),
		"reminder":  reminder(ToolHandoff),
	}
	for name, text := range texts {
		if strings.TrimSpace(text) == "" {
			t.Fatalf("%s prompt is empty", name)
		}
	}
	if !strings.Contains(directorOpening("make it", true), ToolAsk) || strings.Contains(directorOpening("make it", false), ToolAsk) {
		t.Fatal("director opening does not follow the interactive switch")
	}
	for _, want := range []string{"ship it", "tests pass", "no new deps", "docs", "Ship", "T2", "did one", "fix the name", ToolHandoff, "Built before yours: T1"} {
		if !strings.Contains(texts["builder"], want) {
			t.Fatalf("builder prompt lacks %q", want)
		}
	}
	for _, want := range []string{"Linus", "built two", "b.go", "go test", "Sent back 1", ToolVerdict} {
		if !strings.Contains(texts["review"], want) {
			t.Fatalf("review prompt lacks %q", want)
		}
	}
	if strings.Contains(reviewOpening(b, p, tickets[1], h, "Linus", false), "# Brief") {
		t.Fatal("later reviews repeat the brief")
	}
	for verdict, want := range map[Verdict]string{VerdictAccepted: "complete", VerdictFailed: "Stop work", VerdictRework: "done again", VerdictGuidance: "Go on"} {
		if !strings.Contains(verdictText(Decision{Ticket: "T2", Verdict: verdict, Notes: "n"}), want) {
			t.Fatalf("verdict text for %s lacks %q", verdict, want)
		}
	}
	for _, want := range []string{"boom", "T1 one is done by Ada", "Reviewer: fix the name", "Shipped"} {
		if !strings.Contains(texts["report"], want) {
			t.Fatalf("report prompt lacks %q", want)
		}
	}
	for _, want := range []string{"# Plan: Ship", "piece by piece", "- time", "### T2 two", "Depends on: T1", "a.go"} {
		if !strings.Contains(texts["plan"], want) {
			t.Fatalf("plan text lacks %q", want)
		}
	}
	for _, role := range Roles {
		if strings.TrimSpace(roleSystem(role, Default().Team)) == "" || strings.TrimSpace(roleDescription(role)) == "" {
			t.Fatalf("%s has no prompt or description", role)
		}
	}
	for role, wants := range map[Role][]string{
		RoleDirector:  {"Glob finds files", ToolBrief, "# Writing for the person", "without tool calls"},
		RoleArchitect: {"Outline lists", "ends only when you call " + ToolPlan, "within 15 tool calls"},
		RoleBuilder:   {"Edit changes", "Use Shell for commands, builds and tests.", "Plan holds", "ends only when you call " + ToolHandoff, "ask the reviewer through " + ToolHandoff},
		RoleReviewer:  {"Use Shell for commands", "sent back 2 times", "ends only when you call " + ToolVerdict},
	} {
		text := roleSystem(role, Default().Team)
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Fatalf("%s prompt lacks %q:\n%s", role, want, text)
			}
		}
		if strings.Contains(text, "and git") || strings.Contains(text, "Record keeps") || strings.Contains(text, "Agent delegates") {
			t.Fatalf("%s prompt offers a tool it lacks:\n%s", role, text)
		}
	}
	for _, unwanted := range []string{"Shell", "Edit changes"} {
		if strings.Contains(roleSystem(RoleDirector, Default().Team), unwanted) {
			t.Fatalf("director prompt mentions %s", unwanted)
		}
	}
	if !strings.Contains(roleSystem(RoleReviewer, Team{}), "Rework is not allowed") {
		t.Fatal("reviewer prompt ignores a rework cap of zero")
	}
}

func TestLenientLists(t *testing.T) {
	var b Brief
	if err := json.Unmarshal([]byte(`{"objective":"x","acceptance":"tests pass\nbuild is green, no new deps","constraints":["a","b"]}`), &b); err != nil {
		t.Fatal(err)
	}
	if len(b.Acceptance) != 3 || b.Acceptance[1] != "build is green" || len(b.Constraints) != 2 {
		t.Fatalf("lists %+v", b)
	}
	var in planInput
	if err := json.Unmarshal([]byte(`{"title":"t","summary":"s","tickets":[{"title":"a","description":"d","files":"a.go, b.go","depends_on":""}]}`), &in); err != nil {
		t.Fatal(err)
	}
	if len(in.Tickets[0].Files) != 2 || len(in.Tickets[0].DependsOn) != 0 || in.Title != "t" {
		t.Fatalf("ticket lists %+v", in)
	}
	if err := json.Unmarshal([]byte(`{"objective":"x","acceptance":7}`), &b); err == nil {
		t.Fatal("number taken as a list")
	}
}

func TestRouteAndHolders(t *testing.T) {
	if r := route(Vec3{4, 0, 9}, Vec3{7, 0, 11}); len(r) != 1 {
		t.Fatalf("short route %v", r)
	}
	if r := route(builderSpot(0), approach(holder{kind: holdColumn})); len(r) != 3 || r[0].Z != corridorZ || r[1].Z != corridorZ {
		t.Fatalf("crossing route %v", r)
	}
	for i := range 8 {
		d := deskAt(i)
		if d.X < 0 || d.X >= floorW || d.Z < 0 || d.Z >= floorD {
			t.Fatalf("desk %d off the floor at %v", i, d)
		}
	}
	a := holder{kind: holdColumn, column: 1, slot: 7}.spot()
	b := holder{kind: holdColumn, column: 1, slot: 2}.spot()
	if a == b || a.X == b.X && a.Z == b.Z {
		t.Fatalf("column slots %v %v", a, b)
	}
}

func TestRenderer(t *testing.T) {
	fr := &Frame{}
	fr.Resize(64, 32)
	fr.Clear(func(x, y int) RGB { return RGB{} })
	r := &Raster{}
	cam := Camera{Target: Vec3{0, 0, 0}, Yaw: 0, Pitch: 0.5, Dist: 12, FOV: 0.9}
	r.Begin(fr, cam)
	sx, sy, depth, ok := r.Project(Vec3{})
	if !ok || int(sx) != 32 || int(sy) != 16 || depth < 11.9 || depth > 12.1 {
		t.Fatalf("target projects to %v %v depth %v ok %v", sx, sy, depth, ok)
	}
	if _, _, _, ok := r.Project(cam.Eye().Add(Vec3{0, 0, 5})); ok {
		t.Fatal("a point behind the camera projected")
	}
	near := boxQuads(Vec3{-0.5, -0.5, 1}, Vec3{1, 1, 1}, RGB{1, 0, 0}, sideLights(sunDir), nil)
	far := boxQuads(Vec3{-2, -2, -3}, Vec3{4, 4, 1}, RGB{0, 0, 1}, sideLights(sunDir), nil)
	for _, q := range far {
		r.Quad(q)
	}
	for _, q := range near {
		r.Quad(q)
	}
	c := fr.Color[16*fr.W+32]
	if c.R <= c.B {
		t.Fatalf("near red box lost to the far blue one at the center: %+v", c)
	}
	drawn := 0
	for _, d := range fr.Depth {
		if d > 0 {
			drawn++
		}
	}
	if drawn == 0 || drawn == len(fr.Depth) {
		t.Fatalf("%d pixels drawn", drawn)
	}
	solid := [4]RGB{{1, 0, 0}, {1, 0, 0}, {1, 0, 0}, {1, 0, 0}}
	if mask, _, bg := partition(solid); mask != 0 || bg != solid[0] {
		t.Fatalf("solid cell %d %v", mask, bg)
	}
	halves := [4]RGB{{1, 1, 1}, {1, 1, 1}, {0, 0, 0}, {0, 0, 0}}
	mask, fg, bg := partition(halves)
	if (mask != 3 || fg != halves[0] || bg != halves[2]) && (mask != 12 || fg != halves[2] || bg != halves[0]) {
		t.Fatalf("half cell %d %v %v", mask, fg, bg)
	}
	corner := [4]RGB{{0, 1, 0}, {0, 0, 0}, {0, 0, 0}, {0, 0, 0}}
	if mask, fg, _ := partition(corner); mask != 1 && !(mask == 14 && fg == corner[1]) {
		t.Fatalf("corner cell %d %v", mask, fg)
	}
	poly := clipNear([]vertex{{p: Vec3{0, 0, 1}}, {p: Vec3{1, 0, 1}}, {p: Vec3{1, 0, -1}}, {p: Vec3{0, 0, -1}}})
	if len(poly) != 4 {
		t.Fatalf("clipped polygon has %d corners", len(poly))
	}
	for _, v := range poly {
		if v.p.Z < nearPlane-1e-9 {
			t.Fatalf("corner behind the near plane %v", v.p)
		}
	}
	g := grid{}
	g.box(0, 0, 0, 3, 1, 3, RGB{1, 1, 1})
	g.set(1, 1, 1, RGB{1, 1, 1})
	mesh := g.mesh()
	if len(mesh) == 0 {
		t.Fatal("empty mesh")
	}
	for _, q := range mesh {
		for _, l := range q.Light {
			if l <= 0 || l > 2 {
				t.Fatalf("light %v", q.Light)
			}
		}
	}
	if g.occlusion(0, 0, 0, 0, 2) == 0 {
		t.Fatal("the corner of the floor beside the pillar is not occluded")
	}
	q := ansiQuantizer([16]color.Color{})
	if c := q(RGB{1, 1, 1}); c != ansi.IndexedColor(15) {
		t.Fatalf("white quantized to %v", c)
	}
}
