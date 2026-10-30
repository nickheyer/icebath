package sprint

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/harness"
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
	if !lead.Offers("Shell") || lead.Offers(ToolPlan) || lead.Offers("Agent") {
		t.Fatalf("lead tools %+v", lead)
	}
	pm := c.Kind(RolePM)
	if !pm.Offers(ToolResolve) || pm.Offers("Write") || pm.Offers(ToolPlan) {
		t.Fatalf("pm tools %+v", pm)
	}
	eng := c.Kind(RoleEngineer)
	if eng.Offers("WebFetch") || eng.Offers(ToolReview) || !eng.Offers("Edit") {
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
	for name, text := range map[string]string{
		"pm":       pmOpening("make it", true),
		"planner":  plannerOpening(o, 12),
		"engineer": engineerOpening(o, s, tickets, tickets[1], tickets[:1]),
		"lead":     leadOpening(o, s, tickets, 1),
		"closing":  pmClosing("built", tickets[:1], tickets[1:], nil),
		"stopped":  pmClosing("", nil, tickets, errors.New("plan stage: the run ended without calling SprintPlan")),
		"plan":     planText(s, tickets),
		"review":   reviewText(reviewInput{Verdicts: []verdict{{Ticket: "T1", Status: "accepted"}, {Ticket: "T2", Status: "rework", Notes: "fix the name"}}, Summary: "nearly there"}),
		"wrap":     wrapUp(ToolPlan, 30*time.Minute),
	} {
		if strings.TrimSpace(text) == "" {
			t.Fatalf("%s prompt is empty", name)
		}
	}
	eng := engineerOpening(o, s, tickets, tickets[1], tickets[:1])
	for _, want := range []string{"ship it", "tests pass", "no new deps", "Ship", "T2", "did one", "fix the name"} {
		if !strings.Contains(eng, want) {
			t.Fatalf("engineer prompt lacks %q", want)
		}
	}
	if !strings.Contains(leadOpening(o, s, tickets, 1), "round 2") {
		t.Fatal("lead prompt lacks the round")
	}
	plan := planText(s, tickets)
	for _, want := range []string{"# Story: Ship", "T1 one", "T2 two", "do two", "Depends on: T1", "a.go"} {
		if !strings.Contains(plan, want) {
			t.Fatalf("plan text lacks %q", want)
		}
	}
	review := reviewText(reviewInput{Verdicts: []verdict{{Ticket: "T2", Status: "rework", Notes: "fix the name"}}, Summary: "nearly there"})
	for _, want := range []string{"T2 rework: fix the name", "nearly there"} {
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
	s := &Sprint{ID: "sprint-9", Started: time.Now(), Stage: StagePlan}
	s.addMember(Member{Role: RolePlanner, Name: RolePlanner, Live: live})
	register(s)
	if got := unwaited(live.Session.ID(), ToolPlan); !strings.Contains(got, "sprint-9 is in its plan stage") {
		t.Fatalf("running: %s", got)
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
