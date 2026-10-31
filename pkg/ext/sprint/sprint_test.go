package sprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

func TestConfigRoundTrip(t *testing.T) {
	c := Default()
	for _, kv := range [][2]string{
		{"pm.profile", "anthropic"}, {"pm.model", "claude-fable-5-1"}, {"pm.max_tokens", "4096"}, {"engineer.budget", "500000"},
		{"lead.tools", "Read, Shell"}, {"team.max_engineers", "6"}, {"team.tickets_per_engineer", "1.5"}, {"team.parallel", "false"},
		{"limits.build", "45m"}, {"interactive", "false"},
	} {
		if err := c.Set(kv[0], kv[1]); err != nil {
			t.Fatalf("set %s: %v", kv[0], err)
		}
	}
	v := c.Values()
	for _, kv := range [][2]string{{"pm.profile", "anthropic"}, {"pm.max_tokens", "4096"}, {"lead.tools", "Read, Shell"}, {"team.max_engineers", "6"}, {"team.tickets_per_engineer", "1.5"}, {"team.parallel", "false"}, {"limits.build", "45m0s"}, {"interactive", "false"}} {
		if v[kv[0]] != kv[1] {
			t.Fatalf("%s = %q, want %q", kv[0], v[kv[0]], kv[1])
		}
	}
	if c.Limits.Build != Duration(45*time.Minute) || c.Engineer.Budget != 500000 {
		t.Fatalf("values did not land: %+v", c)
	}
	if err := c.Set("pm.max_tokens", ""); err != nil || c.PM.MaxTokens != 0 {
		t.Fatalf("unset max_tokens: %v %d", err, c.PM.MaxTokens)
	}
	if err := c.Set("team.max_engineers", ""); err != nil || c.Team.MaxEngineers != Default().Team.MaxEngineers {
		t.Fatalf("unset max_engineers: %v %d", err, c.Team.MaxEngineers)
	}
	for _, kv := range [][2]string{{"team.max_engineers", "0"}, {"team.tickets_per_engineer", "-1"}, {"limits.plan", "0s"}, {"limits.plan", "soon"}, {"interactive", "maybe"}, {"nothing.here", "1"}} {
		if err := c.Set(kv[0], kv[1]); err == nil {
			t.Fatalf("set %s=%s passed", kv[0], kv[1])
		}
	}
	for _, f := range settingRows(&cards.Env{}) {
		if _, ok := v[f.Key]; !ok {
			t.Fatalf("no value for %s", f.Key)
		}
	}
}

func TestKinds(t *testing.T) {
	c := Default()
	c.Lead.Profile, c.Lead.Model, c.Lead.MaxTokens, c.Lead.MaxRounds = "openai", "gpt-5", 2048, 30
	c.Engineer.Deny = []string{"WebFetch"}
	kinds := c.Kinds()
	if len(kinds) != len(Roles) {
		t.Fatalf("%d kinds", len(kinds))
	}
	for _, k := range kinds {
		if err := k.Validate(); err != nil {
			t.Fatalf("%s: %v", k.Name, err)
		}
	}
	lead := c.Kind(RoleLead)
	if lead.Model == nil || lead.Model.Profile != "openai" || lead.Model.Model != "gpt-5" || lead.MaxTokens != 2048 || lead.MaxRounds != 30 {
		t.Fatalf("lead kind %+v", lead)
	}
	if !lead.Offers("Shell") || lead.Offers(ToolPlan) || lead.Offers("Agent") || lead.Offers("Edit") || lead.Offers("Write") || lead.Offers(ToolReport) || lead.Offers(ToolAsk) {
		t.Fatalf("lead tools %+v", lead)
	}
	if !lead.Offers(ToolDirect) || !lead.Offers(ToolReview) {
		t.Fatalf("lead lacks its tools %+v", lead)
	}
	pm := c.Kind(RolePM)
	if !pm.Offers(ToolResolve) || !pm.Offers(ToolAsk) || pm.Offers("Write") || pm.Offers(ToolPlan) || pm.Offers(ToolDirect) {
		t.Fatalf("pm tools %+v", pm)
	}
	planner := c.Kind(RolePlanner)
	if !planner.Offers(ToolPlan) || planner.Offers(ToolAsk) || planner.Offers(ToolReview) || planner.Offers("Edit") {
		t.Fatalf("planner tools %+v", planner)
	}
	eng := c.Kind(RoleEngineer)
	if eng.Offers("WebFetch") || eng.Offers(ToolReview) || eng.Offers(ToolDirect) || eng.Offers(ToolAsk) || !eng.Offers("Edit") || !eng.Offers(ToolReport) {
		t.Fatalf("engineer tools %+v", eng)
	}
	if role, ok := roleOfKind(pm.Name); !ok || role != RolePM {
		t.Fatalf("role of %s = %q %v", pm.Name, role, ok)
	}
	if _, ok := roleOfKind("review"); ok {
		t.Fatal("foreign kind taken as a role")
	}
}

func TestCheckPlan(t *testing.T) {
	in := planInput{Story: Story{Title: "t", Summary: "s"}, Tickets: []Ticket{{Title: "a", Description: "d"}, {ID: "x", Title: "b", Description: "d", DependsOn: []string{"T1"}}}}
	if err := checkPlan(&in, 5); err != nil {
		t.Fatal(err)
	}
	if in.Tickets[0].ID != "T1" || in.Tickets[1].ID != "x" || in.Tickets[0].Status != TicketTodo {
		t.Fatalf("ids %+v", in.Tickets)
	}
	bad := []planInput{
		{Story: Story{Title: "t", Summary: "s"}},
		{Story: Story{Title: "t"}, Tickets: []Ticket{{Title: "a", Description: "d"}}},
		{Story: Story{Title: "t", Summary: "s"}, Tickets: []Ticket{{ID: "a", Title: "a", Description: "d"}, {ID: "a", Title: "b", Description: "d"}}},
		{Story: Story{Title: "t", Summary: "s"}, Tickets: []Ticket{{ID: "a", Title: "a", Description: "d", DependsOn: []string{"zz"}}}},
		{Story: Story{Title: "t", Summary: "s"}, Tickets: []Ticket{{ID: "a", Title: "a", Description: "d", DependsOn: []string{"b"}}, {ID: "b", Title: "b", Description: "d", DependsOn: []string{"a"}}}},
		{Story: Story{Title: "t", Summary: "s"}, Tickets: []Ticket{{Title: "a", Description: "d"}, {Title: "b", Description: "d"}}},
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

// Sprint in its build with one engineer on a ticket, as the report and direction tools find it
func building(t *testing.T) (*Sprint, *Ticket) {
	t.Helper()
	s := newSprint("sprint-7", "do it", time.Now())
	s.enter(StageBuild, "round 1")
	tk := &Ticket{ID: "T1", Title: "one", Description: "d", Status: TicketActive, Engineer: "engineer-1"}
	s.setPlan(Story{Title: "s", Summary: "x"}, []*Ticket{tk, {ID: "T2", Title: "two", Description: "d", Status: TicketTodo}})
	return s, tk
}

func TestReportsAndDirections(t *testing.T) {
	s, tk := building(t)
	r := &report{Ticket: "T1", Engineer: "engineer-1", Status: ReportQuestion, Text: "which name", Filed: time.Now(), reply: make(chan direction, 1)}
	if err := s.file(context.Background(), &report{Ticket: "T1", Engineer: "engineer-2", Status: ReportDone, Text: "x", reply: make(chan direction, 1)}); err == nil {
		t.Fatal("another engineer filed on the ticket")
	}
	if err := s.file(context.Background(), &report{Ticket: "T2", Engineer: "engineer-1", Status: ReportDone, Text: "x", reply: make(chan direction, 1)}); err == nil {
		t.Fatal("report on a ticket not active")
	}
	if err := s.file(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if s.statusOf(tk) != TicketReported {
		t.Fatalf("ticket after report: %s", tk.Status)
	}
	if err := s.file(context.Background(), &report{Ticket: "T1", Engineer: "engineer-1", Status: ReportDone, Text: "again", reply: make(chan direction, 1)}); err == nil {
		t.Fatal("second report filed on a waiting ticket")
	}
	if got := <-s.inbox; got != r {
		t.Fatal("report did not reach the inbox")
	}
	if _, err := s.direct("T2", direction{Verdict: VerdictAccepted}, 1); err == nil || !strings.Contains(err.Error(), "T1") {
		t.Fatalf("direct on a ticket with no report: %v", err)
	}
	if _, err := s.direct("T1", direction{Verdict: VerdictAccepted}, 1); err == nil {
		t.Fatal("accepted a question report")
	}
	if _, err := s.direct("T1", direction{Verdict: VerdictGuidance, Notes: "call it Foo"}, 1); err != nil {
		t.Fatal(err)
	}
	if d := <-r.reply; d.Verdict != VerdictGuidance || s.statusOf(tk) != TicketActive || len(tk.Notes) != 1 || tk.Round != 0 {
		t.Fatalf("after guidance: %+v %+v", d, tk)
	}
	done := &report{Ticket: "T1", Engineer: "engineer-1", Status: ReportDone, Text: "built", Filed: time.Now(), reply: make(chan direction, 1)}
	if err := s.file(context.Background(), done); err != nil {
		t.Fatal(err)
	}
	<-s.inbox
	if _, err := s.direct("T1", direction{Verdict: VerdictRework, Notes: "tests"}, 1); err != nil {
		t.Fatal(err)
	}
	if d := <-done.reply; d.Verdict != VerdictRework || tk.Round != 1 || s.statusOf(tk) != TicketActive {
		t.Fatalf("after rework: %+v %+v", d, tk)
	}
	again := &report{Ticket: "T1", Engineer: "engineer-1", Status: ReportDone, Text: "built again", Filed: time.Now(), reply: make(chan direction, 1)}
	if err := s.file(context.Background(), again); err != nil {
		t.Fatal(err)
	}
	<-s.inbox
	if _, err := s.direct("T1", direction{Verdict: VerdictRework, Notes: "more"}, 1); err == nil || !strings.Contains(err.Error(), "most the sprint allows") {
		t.Fatalf("rework past the cap: %v", err)
	}
	s.lateRound.Store(true)
	if _, err := s.direct("T1", direction{Verdict: VerdictRework, Notes: "more"}, 5); err == nil || !strings.Contains(err.Error(), "past its limit") {
		t.Fatalf("rework in a late round: %v", err)
	}
	if _, err := s.direct("T1", direction{Verdict: VerdictAccepted}, 1); err != nil {
		t.Fatal(err)
	}
	if d := <-again.reply; d.Verdict != VerdictAccepted || s.statusOf(tk) != TicketDone || tk.Report != "built again" || s.directions() != 3 {
		t.Fatalf("after accepted: %+v %+v %d", d, tk, s.directions())
	}
	if len(s.openReports()) != 0 {
		t.Fatal("reports left open")
	}
}

func TestWithdrawAndLoseLead(t *testing.T) {
	s, tk := building(t)
	r := &report{Ticket: "T1", Engineer: "engineer-1", Status: ReportBlocked, Text: "stuck", Filed: time.Now(), reply: make(chan direction, 1)}
	if err := s.file(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	<-s.inbox
	if !s.withdraw(r) || s.statusOf(tk) != TicketActive || len(s.openReports()) != 0 {
		t.Fatalf("withdraw: %+v", tk)
	}
	if s.withdraw(r) {
		t.Fatal("withdrew twice")
	}
	r = &report{Ticket: "T1", Engineer: "engineer-1", Status: ReportDone, Text: "built", Filed: time.Now(), reply: make(chan direction, 1)}
	if err := s.file(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	<-s.inbox
	if n := s.loseLead("the lead failed: boom"); n != 1 {
		t.Fatalf("failed %d reports", n)
	}
	if d := <-r.reply; d.Verdict != VerdictFailed || s.statusOf(tk) != TicketFailed || !strings.Contains(tk.Err, "boom") {
		t.Fatalf("after losing the lead: %+v %+v", d, tk)
	}
	t2 := s.ticket("T2")
	s.update(t2, func(t *Ticket) { t.Status, t.Engineer = TicketActive, "engineer-1" })
	if err := s.file(context.Background(), &report{Ticket: "T2", Engineer: "engineer-1", Status: ReportDone, Text: "x", reply: make(chan direction, 1)}); err == nil || !strings.Contains(err.Error(), "nobody can direct") {
		t.Fatalf("report after the lead is gone: %v", err)
	}
}

func TestTransient(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, agent.ErrInterrupted, errBudget, errSilent, errExpired, fmt.Errorf("agent: 50 %w", agent.ErrRounds), fmt.Errorf("wrapped: %w", errBudget), &protocol.Error{Status: 401, Auth: true}, nil} {
		if transient(err) {
			t.Fatalf("%v taken as transient", err)
		}
	}
	for _, err := range []error{errors.New("stream ended without a done event"), &protocol.Error{Status: 503, Transient: true}, fmt.Errorf("harness: a run is going on s1"), errors.New("connection reset")} {
		if !transient(err) {
			t.Fatalf("%v not taken as transient", err)
		}
	}
}

func TestRestartResetsTheSprint(t *testing.T) {
	s, tk := building(t)
	s.setObjective(Objective{Objective: "x"})
	s.setBriefing("fit the pieces")
	live := &harness.Live{Session: session.New()}
	s.putMember(Member{Role: RoleLead, Name: RoleLead, Live: live})
	s.putMember(Member{Role: RoleLead, Name: RoleLead, Live: &harness.Live{Session: session.New()}})
	if n := len(s.Snapshot().Members); n != 1 {
		t.Fatalf("%d members after putting the same name twice", n)
	}
	r := &report{Ticket: "T1", Engineer: "engineer-1", Status: ReportDone, Text: "built", Filed: time.Now(), reply: make(chan direction, 1)}
	if err := s.file(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	<-s.inbox
	t2 := s.ticket("T2")
	s.update(t2, func(t *Ticket) { t.Status, t.Err = TicketFailed, "failed by the lead: hopeless" })
	s.setPlan(s.Story, append(s.Tickets, &Ticket{ID: "T3", Title: "three", Description: "d", Status: TicketFailed, Err: "was stopped: context canceled"}, &Ticket{ID: "T4", Title: "four", Description: "d", Status: TicketDone}))
	s.enter(StageReview, "round 1")
	s.finish(context.Canceled, "")
	snap := s.Snapshot()
	if snap.Running() || !snap.Stopped() || !snap.Restartable() || snap.Last != StageReview || restartStage(snap) != StageBuild {
		t.Fatalf("stopped snapshot %+v", snap)
	}
	if from := s.restart(); from != StageBuild {
		t.Fatalf("restart from %s", from)
	}
	if d := <-r.reply; d.Verdict != VerdictFailed {
		t.Fatalf("open report on restart: %+v", d)
	}
	snap = s.Snapshot()
	if !snap.Running() || snap.Stage != StageBuild || snap.Paused != "" || snap.Err != nil {
		t.Fatalf("restarted snapshot %+v", snap)
	}
	want := map[string]TicketStatus{"T1": TicketTodo, "T2": TicketFailed, "T3": TicketTodo, "T4": TicketDone}
	for _, tk := range snap.Tickets {
		if tk.Status != want[tk.ID] {
			t.Fatalf("ticket %s is %s after restart, want %s", tk.ID, tk.Status, want[tk.ID])
		}
	}
	if tk.Err != "" || len(s.openReports()) != 0 {
		t.Fatalf("ticket after restart %+v, %d reports open", tk, len(s.openReports()))
	}
	s.pruneMembers(func(m Member) bool { return false })
	if len(s.Snapshot().Members) != 0 {
		t.Fatal("closed members kept")
	}
	s.finish(nil, "")
	if snap := s.Snapshot(); snap.Restartable() || snap.Last != StageDone {
		t.Fatalf("done snapshot %+v", snap)
	}
	bare := newSprint("sprint-8", "x", time.Now())
	bare.enter(StageBuild, "round 1")
	bare.finish(errors.New("boom"), "")
	if from := bare.restart(); from != StageRefine {
		t.Fatalf("a sprint with no objective restarts at %s", from)
	}
}

func TestCheckReview(t *testing.T) {
	s, tk := building(t)
	s.update(tk, func(t *Ticket) { t.Status, t.Round = TicketDone, 1 })
	snap := s.Snapshot()
	if err := checkReview(reviewInput{Summary: "ok", Rework: []sendBack{{Ticket: "T2", Notes: "fix"}}}, snap, 1); err != nil {
		t.Fatal(err)
	}
	bad := []reviewInput{
		{Summary: "ok", Rework: []sendBack{{Ticket: "T9", Notes: "fix"}}},
		{Summary: "ok", Rework: []sendBack{{Ticket: "T2", Notes: "fix"}, {Ticket: "T2", Notes: "again"}}},
		{Summary: "ok", Rework: []sendBack{{Ticket: "T2", Notes: " "}}},
		{Summary: "ok", Rework: []sendBack{{Ticket: "T1", Notes: "fix"}}},
	}
	for i, in := range bad {
		if err := checkReview(in, snap, 1); err == nil {
			t.Fatalf("review %d passed", i)
		}
	}
}

func TestHandoffDelivery(t *testing.T) {
	w := await("sess", ToolPlan)
	defer w.release("sess")
	if err := deliver("sess", ToolResolve, Objective{}); err == nil {
		t.Fatal("wrong tool delivered")
	}
	if err := deliver("other", ToolPlan, planInput{}); err == nil {
		t.Fatal("wrong session delivered")
	}
	if err := deliver("sess", ToolPlan, planInput{Story: Story{Title: "x"}}); err != nil {
		t.Fatal(err)
	}
	if err := deliver("sess", ToolPlan, planInput{}); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("second delivery: %v", err)
	}
	if v := <-w.ch; v.(planInput).Story.Title != "x" {
		t.Fatalf("got %+v", v)
	}
}

func TestPrompts(t *testing.T) {
	o := Objective{Objective: "ship it", Acceptance: []string{"tests pass"}, Constraints: []string{"no new deps"}}
	s := Story{Title: "Ship", Summary: "all of it", Size: "medium"}
	tickets := []Ticket{{ID: "T1", Title: "one", Description: "do one", Files: []string{"a.go"}, Report: "did one", Status: TicketDone, Engineer: "engineer-1"}, {ID: "T2", Title: "two", Description: "do two", DependsOn: []string{"T1"}, Notes: []string{"fix the name"}}}
	reports := []*report{{Ticket: "T2", Engineer: "engineer-2", Status: ReportDone, Text: "built two"}}
	for name, text := range map[string]string{
		"pm":        pmOpening("make it", true),
		"planner":   plannerOpening(o, 12),
		"brief":     leadBriefOpening(o, s, tickets, 2),
		"engineer":  engineerOpening(o, s, "keep the names", tickets, tickets[1], tickets[:1]),
		"reports":   reportsText(reports),
		"nudge":     nudgeDirect(reports),
		"direction": directionText("T2", direction{Verdict: VerdictRework, Notes: "tests"}),
		"review":    reviewOpening(o, s, tickets, 1),
		"closing":   pmClosing("built", tickets[:1], tickets[1:], nil),
		"stopped":   pmClosing("", nil, tickets, errors.New("plan stage: the run ended without calling SprintPlan")),
		"plan":      planText(s, tickets),
		"verdicts":  reviewText(reviewInput{Summary: "nearly there", Rework: []sendBack{{Ticket: "T2", Notes: "fix the name"}}}),
		"wrap":      wrapUp(ToolPlan, 30*time.Minute),
		"late":      wrapUpDirect(90 * time.Minute),
	} {
		if strings.TrimSpace(text) == "" {
			t.Fatalf("%s prompt is empty", name)
		}
	}
	if !strings.Contains(pmOpening("make it", true), ToolAsk) || strings.Contains(pmOpening("make it", false), ToolAsk) {
		t.Fatal("pm opening does not follow the interactive switch")
	}
	if strings.Contains(plannerOpening(o, 12), ToolAsk) || strings.Contains(plannerSystem, ToolAsk) {
		t.Fatal("planner prompts offer the ask tool")
	}
	eng := engineerOpening(o, s, "keep the names", tickets, tickets[1], tickets[:1])
	for _, want := range []string{"ship it", "tests pass", "no new deps", "Ship", "T2", "did one", "fix the name", "keep the names", ToolReport} {
		if !strings.Contains(eng, want) {
			t.Fatalf("engineer prompt lacks %q", want)
		}
	}
	if brief := leadBriefOpening(o, s, tickets, 2); !strings.Contains(brief, "2 engineers") || !strings.Contains(brief, "do two") {
		t.Fatalf("brief prompt: %s", brief)
	}
	if text := reportsText(reports); !strings.Contains(text, "engineer-2") || !strings.Contains(text, "built two") || !strings.Contains(text, ToolDirect) {
		t.Fatalf("reports text: %s", text)
	}
	for verdict, want := range map[string]string{VerdictAccepted: "complete", VerdictFailed: "Stop work", VerdictRework: "report done again", VerdictGuidance: "Go on"} {
		if !strings.Contains(directionText("T2", direction{Verdict: verdict, Notes: "n"}), want) {
			t.Fatalf("direction text for %s lacks %q", verdict, want)
		}
	}
	if !strings.Contains(reviewOpening(o, s, tickets, 1), "round 2") {
		t.Fatal("review prompt lacks the round")
	}
	plan := planText(s, tickets)
	for _, want := range []string{"# Story: Ship", "T1 one", "T2 two", "do two", "Depends on: T1", "a.go"} {
		if !strings.Contains(plan, want) {
			t.Fatalf("plan text lacks %q", want)
		}
	}
	review := reviewText(reviewInput{Summary: "nearly there", Rework: []sendBack{{Ticket: "T2", Notes: "fix the name"}}})
	for _, want := range []string{"T2: fix the name", "nearly there"} {
		if !strings.Contains(review, want) {
			t.Fatalf("review text lacks %q", want)
		}
	}
	if !strings.Contains(pmClosing("", nil, tickets, errors.New("boom")), "boom") {
		t.Fatal("closing prompt lacks the failure")
	}
}

func TestLenientLists(t *testing.T) {
	var o Objective
	if err := json.Unmarshal([]byte(`{"objective":"x","acceptance":"tests pass\nbuild is green, no new deps","constraints":["a","b"]}`), &o); err != nil {
		t.Fatal(err)
	}
	if len(o.Acceptance) != 3 || o.Acceptance[1] != "build is green" || len(o.Constraints) != 2 {
		t.Fatalf("lists %+v", o)
	}
	var in planInput
	if err := json.Unmarshal([]byte(`{"story":{"title":"t","summary":"s"},"tickets":[{"title":"a","description":"d","files":"a.go, b.go","depends_on":""}]}`), &in); err != nil {
		t.Fatal(err)
	}
	if len(in.Tickets[0].Files) != 2 || len(in.Tickets[0].DependsOn) != 0 {
		t.Fatalf("ticket lists %+v", in.Tickets[0])
	}
	if err := json.Unmarshal([]byte(`{"objective":"x","acceptance":7}`), &o); err == nil {
		t.Fatal("number taken as a list")
	}
}

func TestUnwaited(t *testing.T) {
	if got := unwaited("nobody", ToolPlan); !strings.Contains(got, "not part of a sprint") || !strings.Contains(got, "/"+Name) {
		t.Fatalf("stranger: %s", got)
	}
	live := &harness.Live{Session: session.New()}
	s := newSprint("sprint-9", "x", time.Now())
	s.enter(StagePlan, "cutting")
	s.putMember(Member{Role: RolePlanner, Name: RolePlanner, Live: live})
	register(s)
	if got := unwaited(live.Session.ID(), ToolPlan); !strings.Contains(got, "sprint-9 is in its plan stage") {
		t.Fatalf("running: %s", got)
	}
	if found, m, ok := sprintOfSession(live.Session.ID()); !ok || found != s || m.Name != RolePlanner {
		t.Fatalf("sprint of session: %v %+v %v", found, m, ok)
	}
	s.finish(errors.New("plan stage: the run ended without calling SprintPlan"), "")
	got := unwaited(live.Session.ID(), ToolPlan)
	for _, want := range []string{"sprint-9 ended", "failed: plan stage", "/" + Name} {
		if !strings.Contains(got, want) {
			t.Fatalf("ended: %s", got)
		}
	}
	if err := deliver(live.Session.ID(), ToolPlan, planInput{}); err == nil || !strings.Contains(err.Error(), "sprint-9 ended") {
		t.Fatalf("deliver after the end: %v", err)
	}
	w := await("s", ToolReview)
	defer w.release("s")
	if err := deliver("s", ToolPlan, planInput{}); err == nil || !strings.Contains(err.Error(), "waits for SprintReview, not SprintPlan") {
		t.Fatalf("wrong tool: %v", err)
	}
}
