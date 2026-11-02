package sprint

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/vxl"
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
	if c.NameOf(RoleBuilder, 1) != "Bob" || c.NameOf(RoleBuilder, 2) != "Builder 3" || c.NameOf(RoleDirector, 0) != "Director" {
		t.Fatalf("names %q %q %q", c.NameOf(RoleBuilder, 1), c.NameOf(RoleBuilder, 2), c.NameOf(RoleDirector, 0))
	}
	if c.LookOf(RoleBuilder, 1) != (Look{"rose", "halo"}) || c.LookOf(RoleBuilder, 0) != (Look{"teal", "crest"}) || c.LookOf(RoleBuilder, 4) != defaultBuilderLooks[4] || c.LookOf(RoleDirector, 0) != (Look{"gold", "hood"}) {
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
	kinds := c.Kinds("")
	if len(kinds) != len(Roles) {
		t.Fatalf("%d kinds", len(kinds))
	}
	for _, k := range kinds {
		if err := k.Validate(); err != nil {
			t.Fatalf("%s: %v", k.Name, err)
		}
	}
	reviewer := c.Kind(RoleReviewer, "")
	if reviewer.Model == nil || reviewer.Model.Profile != "openai" || reviewer.Model.Model != "gpt-5" || reviewer.MaxTokens != 2048 || reviewer.MaxRounds != 30 {
		t.Fatalf("reviewer kind %+v", reviewer)
	}
	if !reviewer.Offers("Shell") || !reviewer.Offers(ToolVerdict) || reviewer.Offers("Edit") || reviewer.Offers(ToolHandoff) || reviewer.Offers("Agent") {
		t.Fatalf("reviewer tools %+v", reviewer)
	}
	director := c.Kind(RoleDirector, "")
	if !director.Offers(ToolBrief) || !director.Offers(ToolAsk) || director.Offers("Write") || director.Offers(ToolPlan) {
		t.Fatalf("director tools %+v", director)
	}
	architect := c.Kind(RoleArchitect, "")
	if !architect.Offers(ToolPlan) || architect.Offers(ToolAsk) || architect.Offers("Edit") || architect.Offers("Shell") {
		t.Fatalf("architect tools %+v", architect)
	}
	builder := c.Kind(RoleBuilder, "")
	if !builder.Offers("Edit") || !builder.Offers("mcp__x__y") || !builder.Offers(ToolHandoff) || builder.Offers("Agent") || builder.Offers(ToolVerdict) || builder.Offers(ToolBrief) {
		t.Fatalf("builder tools %+v", builder)
	}
	if reviewer.PruneAfter != 30*(1+maxReminders) || architect.MaxRounds != 20 || architect.PruneAfter != 20*(1+maxReminders) || director.MaxRounds != 10 || director.PruneAfter != 10*(1+maxReminders) || builder.MaxRounds != 0 || builder.PruneAfter != 0 {
		t.Fatalf("round caps and prune windows: reviewer %d/%d architect %d/%d director %d/%d builder %d/%d", reviewer.MaxRounds, reviewer.PruneAfter, architect.MaxRounds, architect.PruneAfter, director.MaxRounds, director.PruneAfter, builder.MaxRounds, builder.PruneAfter)
	}
	c.Roles[RoleBuilder] = RoleConfig{Model: "kairi"}
	if k := c.Kind(RoleBuilder, "home"); k.Model == nil || k.Model.Profile != "home" || k.Model.Model != "kairi" {
		t.Fatalf("builder model without a profile %+v", k.Model)
	}
	if k := c.Kind(RoleBuilder, ""); k.Model != nil {
		t.Fatalf("builder model with no default profile %+v", k.Model)
	}
	for _, s := range Settings() {
		if strings.HasSuffix(s.Key, ".profile") != s.Profile {
			t.Fatalf("%s marked as a profile: %v", s.Key, s.Profile)
		}
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
	reported := newSprint("sprint-11", "x", time.Now(), nil)
	reported.setBrief(Brief{Objective: "x"})
	reported.setPlan(Plan{Title: "p", Summary: "s"}, []*Ticket{{ID: "T1", Title: "a", Description: "d", Status: TicketTodo}})
	reported.enter(StageReport, "writing")
	reported.setCause(errors.New("build stage: no builder"))
	reported.finish(errors.New("boom"))
	if from := restartStage(reported.Snapshot()); from != StageBuild {
		t.Fatalf("a report over unbuilt tickets restarts at %s", from)
	}
	if from := reported.restart(); from != StageBuild || reported.Snapshot().Cause != "" {
		t.Fatalf("restart at %s keeping cause %q", from, reported.Snapshot().Cause)
	}
	settled := newSprint("sprint-12", "x", time.Now(), nil)
	settled.setBrief(Brief{Objective: "x"})
	settled.setPlan(Plan{Title: "p", Summary: "s"}, []*Ticket{{ID: "T1", Title: "a", Description: "d", Status: TicketDone}, {ID: "T2", Title: "b", Description: "d", Status: TicketFailed, Err: reviewerFailed + "no"}})
	settled.enter(StageReport, "writing")
	settled.setCause(errors.New("build stage: half"))
	settled.finish(errors.New("boom"))
	if from := settled.restart(); from != StageReport || settled.Snapshot().Cause == "" {
		t.Fatalf("restart at %s with cause %q", from, settled.Snapshot().Cause)
	}
}

func TestReportText(t *testing.T) {
	if got := reportText(protocol.Response{Content: []protocol.Part{protocol.Text{Text: " " + agent.Omitted + " "}}}); got != "" {
		t.Fatalf("omitted reasoning taken as a report: %q", got)
	}
	if got := reportText(protocol.Response{}); got != "" {
		t.Fatalf("empty turn taken as a report: %q", got)
	}
	if got := reportText(protocol.Response{Content: []protocol.Part{protocol.Text{Text: "# Shipped\n"}}}); got != "# Shipped" {
		t.Fatalf("report %q", got)
	}
}

func TestCheckTeamFailsBeforeTheBrief(t *testing.T) {
	t.Setenv(profiles.ConfigDirEnv, t.TempDir())
	path, err := profiles.UserPath()
	if err != nil {
		t.Fatal(err)
	}
	doc := profiles.Document{Default: "home", Profiles: []profiles.Profile{{Name: "home", Template: "anthropic", Key: profiles.Source{Kind: profiles.SourceStatic, Value: "k"}}}}
	if err := profiles.WriteFile(path, doc); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	app, err := harness.Build(ctx, harness.Config{Profile: "home"}, func(any) {}, harness.BuildOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	cfg := Default()
	cfg.Roles[RoleReviewer] = RoleConfig{Profile: "gone", Model: "m"}
	r := &runner{cfg: cfg, f: &harness.Flow{ID: "sprint-1", Name: Name, App: app}, s: newSprint("sprint-1", "x", time.Now(), nil)}
	err = r.checkTeam(ctx)
	if err == nil || !strings.Contains(err.Error(), "Reviewer") || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("unknown reviewer profile passed: %v", err)
	}
	msgs := r.s.Snapshot().Messages
	if len(msgs) != 1 || msgs[0].Kind != MsgFailure || msgs[0].To != Person || !strings.Contains(msgs[0].Text, "gone") {
		t.Fatalf("failure not told to the person: %+v", msgs)
	}
	cfg.Roles[RoleReviewer] = RoleConfig{Model: app.Primary.Model}
	r.cfg = cfg
	if err := r.checkTeam(ctx); err != nil {
		t.Fatal(err)
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
		"report":    reportOpening(b, p, tickets, "build stage: boom"),
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

// Platform cut from a plan puts every node on a level after its dependencies, with walkable spots and conduits between them
func TestDiveLayout(t *testing.T) {
	plan := []planNode{{id: "T1"}, {id: "T2"}, {id: "T3", deps: []string{"T1", "T2"}}, {id: "T4", deps: []string{"T3"}}, {id: "T5", deps: []string{"T9"}}}
	lay := buildDive(plan, 8)
	if lay.levels["T1"] != 0 || lay.levels["T3"] != 1 || lay.levels["T4"] != 2 || lay.levels["T5"] != 0 {
		t.Fatalf("levels %v", lay.levels)
	}
	if lay.nodes["T4"].Z >= lay.nodes["T3"].Z || lay.nodes["T3"].Z >= lay.nodes["T1"].Z {
		t.Fatal("deeper levels do not lie further north")
	}
	for id, p := range lay.nodes {
		if !lay.tiles[cellOf(p)] || lay.walkable(int(p.X), int(p.Z)) {
			t.Fatalf("node %s pedestal at %v is not on a blocked tile", id, p)
		}
		at, _ := lay.jackSpot(id)
		if !lay.walkable(int(at.X), int(at.Z)) {
			t.Fatalf("jack spot of %s at %v is not walkable", id, at)
		}
	}
	if len(lay.conduits[[2]string{"T3", "T1"}]) == 0 || len(lay.conduits[[2]string{"T5", "T9"}]) != 0 {
		t.Fatalf("conduits %v", lay.conduits)
	}
	if len(lay.cradles) != 8 || len(lay.perches) == 0 {
		t.Fatalf("%d cradles %d perches", len(lay.cradles), len(lay.perches))
	}
	for i, c := range lay.cradles {
		if !lay.tiles[cellOf(c)] {
			t.Fatalf("cradle %d at %v hangs over the void", i, c)
		}
	}
	from, to := lay.cradle(0), lay.spireFoot()
	path := lay.path(from, to)
	if len(path) == 0 || path[len(path)-1] != to {
		t.Fatalf("path %v", path)
	}
	prev := from
	for _, p := range path {
		for _, c := range vxl.LineCells(int(prev.X), int(prev.Z), int(p.X), int(p.Z)) {
			if !lay.walkable(c[0], c[1]) {
				t.Fatalf("path crosses blocked tile %v between %v and %v", c, prev, p)
			}
		}
		prev = p
	}
	empty := buildDive(nil, 1)
	if len(empty.nodes) != 0 || empty.w <= 0 || empty.d <= landingDepth {
		t.Fatalf("empty dive %+v", empty)
	}
	var wide []planNode
	for i := range 14 {
		wide = append(wide, planNode{id: "W" + string(rune('a'+i))})
	}
	wide = append(wide, planNode{id: "X", deps: []string{"Wa", "Wn"}})
	flat := buildDive(wide, 8)
	if flat.w >= 50 || flat.d >= 50 {
		t.Fatalf("fourteen nodes on one level spread the platform to %d by %d", flat.w, flat.d)
	}
	rowsAt := map[float64]int{}
	for id, p := range flat.nodes {
		if id != "X" {
			rowsAt[p.Z]++
		}
		at, _ := flat.jackSpot(id)
		if !flat.walkable(int(at.X), int(at.Z)) || at.Flat(p) > 4 {
			t.Fatalf("jack spot of %s at %v is not walkable beside its node at %v", id, at, p)
		}
	}
	if len(rowsAt) != 3 {
		t.Fatalf("fourteen nodes wrapped onto %d rows", len(rowsAt))
	}
	for z, n := range rowsAt {
		if n > rowNodes || flat.nodes["X"].Z >= z {
			t.Fatalf("row at %v holds %d nodes, the dependent sits at %v", z, n, flat.nodes["X"].Z)
		}
	}
	for _, dep := range []string{"Wa", "Wn"} {
		cells := flat.conduits[[2]string{"X", dep}]
		if len(cells) == 0 || len(flat.runs[[2]string{"X", dep}]) > 5 {
			t.Fatalf("conduit from %s has %d cells in %d runs", dep, len(cells), len(flat.runs[[2]string{"X", dep}]))
		}
		for _, c := range cells {
			if flat.blocked[c] {
				t.Fatalf("conduit from %s runs under a pedestal at %v", dep, c)
			}
		}
	}
	if rig, ok := hatRigs["fedora"]; !ok || rig != "hood" {
		t.Fatal("old hats do not map to rigs")
	}
}

// Crew dropped on the platform settle into their duties and leave it together at the end
func TestSceneChoreography(t *testing.T) {
	sc := newScene(3)
	snap := probeSnapshot()
	cfg := Default()
	live := probeLive()
	for i := range 240 {
		if i%12 == 0 {
			sc.sync(&snap, live, cfg)
		}
		sc.update(1.0 / 30)
	}
	if ow := sc.byName["Overwatch"]; ow.pos.Y < spireHeight-0.1 || ow.state != stateReview {
		t.Fatalf("the overwatch is at %v in state %v, not reviewing from the spire", ow.pos, ow.state)
	}
	if linus := sc.byName["Linus"]; linus.duty.kind != dutyWaitNode || linus.state != stateWait {
		t.Fatalf("Linus duty %v state %v, not waiting at his node", linus.duty.kind, linus.state)
	}
	snap.Messages = append(snap.Messages, Message{Seq: 3, At: time.Now(), Kind: MsgVerdict, From: "Overwatch", To: "Linus", Ticket: "T2", Text: "accepted: fine"})
	snap.Tickets[1].Status = TicketDone
	sc.sync(&snap, live, cfg)
	if len(sc.packets) != 1 {
		t.Fatalf("%d packets after a verdict", len(sc.packets))
	}
	for range 60 {
		sc.update(1.0 / 30)
	}
	if linus := sc.byName["Linus"]; len(sc.packets) != 0 || linus.emote != "✓" {
		t.Fatalf("Linus did not take the accepted verdict: emote %q state %v", linus.emote, linus.state)
	}
	if n := sc.nodes["T2"]; n.status != TicketDone || n.shell > 0.5 {
		t.Fatalf("T2 node %v shell %v after acceptance", n.status, n.shell)
	}
	snap.Finished = time.Now()
	snap.Stage = StageDone
	sc.sync(&snap, map[string]liveState{}, cfg)
	if !sc.ended || sc.endMode != endCheer || len(sc.queue) != len(sc.actors) {
		t.Fatalf("ended %v mode %v queue %d", sc.ended, sc.endMode, len(sc.queue))
	}
	for i := range 3600 {
		sc.update(1.0 / 30)
		if i%30 == 0 {
			sc.sync(&snap, map[string]liveState{}, cfg)
		}
	}
	gone := 0
	for _, a := range sc.actors {
		if a.gone {
			gone++
		}
	}
	if gone != len(sc.actors) {
		for _, a := range sc.actors {
			if !a.gone {
				t.Logf("%s state=%d duty=%d at=%v pos=%v steps=%d exit=%.2f landed=%v partner=%v ambient=%q cool=%.1f leap=%.2f queueHead=%v", a.name, a.state, a.duty.kind, a.duty.at, a.pos, len(a.steps), a.exit, a.landed, a.partner != nil, a.ambient, a.cool, a.leapT, len(sc.queue) > 0 && sc.queue[0] == a)
				for _, st := range a.steps {
					t.Logf("   step kind=%d to=%v dur=%.1f state=%d", st.kind, st.to, st.dur, st.state)
				}
			}
		}
		t.Fatalf("%d of %d jacked out after the sprint was done", gone, len(sc.actors))
	}
	snap.Finished = time.Time{}
	snap.Stage = StageBuild
	sc.sync(&snap, live, cfg)
	for range 90 {
		sc.update(1.0 / 30)
	}
	for _, a := range sc.actors {
		if a.gone || !a.landed {
			t.Fatalf("%s did not come back for the restart", a.name)
		}
	}
	sc.sync(nil, nil, cfg)
	if sc.lobby == nil || len(sc.nodes) != 0 {
		t.Fatalf("lobby %v nodes %d", sc.lobby, len(sc.nodes))
	}
	for range 600 {
		sc.update(1.0 / 30)
	}
	if !sc.lay.onPlatform(sc.lobby.pos) {
		t.Fatalf("the lobby fixer wandered off at %v", sc.lobby.pos)
	}
}
