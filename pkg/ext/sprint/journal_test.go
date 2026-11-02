package sprint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/session"
)

// App on a stub profile with its sessions under a fresh directory
func testApp(t *testing.T) *harness.App {
	t.Helper()
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
	t.Cleanup(func() { app.Close(ctx) })
	return app
}

// Drops a sprint from the registry at the end of the test
func unregister(t *testing.T, s *Sprint) {
	t.Helper()
	t.Cleanup(func() {
		regMu.Lock()
		for i, have := range sprints {
			if have == s {
				sprints = append(sprints[:i], sprints[i+1:]...)
				break
			}
		}
		regMu.Unlock()
	})
}

// Sprint file written on every change and read back whole, the stop of the person kept as a stop
func TestJournalRoundTrip(t *testing.T) {
	app := testApp(t)
	ctx := context.Background()
	s := newSprint("sprint-3", "ship it", time.Now(), nil)
	if err := s.keep(ctx, app); err != nil {
		t.Fatal(err)
	}
	s.setBrief(Brief{Objective: "x", Acceptance: []string{"a"}})
	s.setPlan(Plan{Title: "p", Summary: "s"}, []*Ticket{{ID: "T1", Title: "one", Description: "d", Status: TicketActive, Builder: "Builder 1"}, {ID: "T2", Title: "two", Description: "d", Status: TicketTodo, DependsOn: []string{"T1"}}})
	s.putMember(&Member{Role: RoleBuilder, Name: "Builder 1", Session: "20260101-000000-abcdefabcdefabcd", Joined: time.Now()})
	s.say(MsgAssign, "Reviewer", "Builder 1", "T1", "take T1")
	s.spend("Builder 1", 4321)
	s.finish(context.Canceled)
	dir, err := journalPath(app)
	if err != nil {
		t.Fatal(err)
	}
	r, err := readRecord(filepath.Join(dir, "sprint-3.json"))
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != "sprint-3" || r.Project != app.Root || !r.Stopped || r.Finished.IsZero() || len(r.Tickets) != 2 || len(r.Members) != 1 || r.Members[0].Session == "" || r.Spent["Builder 1"] != 4321 || len(r.Messages) != 1 {
		t.Fatalf("record %+v", r)
	}
	back := fromRecord(r, time.Now(), dir, nil)
	snap := back.Snapshot()
	if !snap.Stopped() || snap.Exited() || snap.Running() || snap.Brief.Objective != "x" || snap.Last != StageBrief || len(snap.MessagesAfter(0)) != 1 {
		t.Fatalf("snapshot back %+v", snap)
	}
	if m, ok := snap.MemberOfSession("20260101-000000-abcdefabcdefabcd"); !ok || m.Name != "Builder 1" {
		t.Fatalf("member by stored session %+v %v", m, ok)
	}
	if err := back.unkeep(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sprint-3.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file kept after unkeep: %v", err)
	}
}

// Sprint whose process died comes back interrupted at its stage, its tickets freed on restart, and the ids go on counting
func TestRecoverAfterExit(t *testing.T) {
	app := testApp(t)
	dir, err := journalPath(app)
	if err != nil {
		t.Fatal(err)
	}
	r := record{ID: "sprint-7", Project: app.Root, Request: "build it", Started: time.Now().Add(-time.Hour), Stage: StageBuild, Status: "building", Brief: Brief{Objective: "o"}, Plan: Plan{Title: "p", Summary: "s"},
		Tickets: []Ticket{{ID: "T1", Title: "a", Description: "d", Status: TicketDone}, {ID: "T2", Title: "b", Description: "d", Status: TicketActive, Builder: "Builder 1"}, {ID: "T3", Title: "c", Description: "d", Status: TicketFailed, Err: reviewerFailed + "no"}},
		Members: []memberRecord{{Role: RoleDirector, Name: "Director", Session: "s1"}, {Role: RoleBuilder, Name: "Builder 1", Session: "s2", Ticket: "T2", Task: "building T2"}}}
	if err := writeRecord(filepath.Join(dir, "sprint-7.json"), r); err != nil {
		t.Fatal(err)
	}
	other := r
	other.ID, other.Project = "sprint-9", "/elsewhere"
	if err := writeRecord(filepath.Join(dir, "sprint-9.json"), other); err != nil {
		t.Fatal(err)
	}
	n, err := recoverSprints(app)
	if err != nil || n != 1 {
		t.Fatalf("recovered %d %v", n, err)
	}
	s := sprintByID("sprint-7")
	if s == nil {
		t.Fatal("sprint-7 not registered")
	}
	unregister(t, s)
	snap := s.Snapshot()
	if snap.Running() || !snap.Exited() || !snap.Stopped() || !snap.Restartable() || snap.Last != StageBuild || !strings.Contains(snap.Status, "restart") {
		t.Fatalf("recovered snapshot %+v", snap)
	}
	if m, ok := snap.Member("Builder 1"); !ok || m.Ticket != "" || m.SessionID() != "s2" {
		t.Fatalf("recovered member %+v %v", m, ok)
	}
	if from := s.restart(); from != StageBuild {
		t.Fatalf("restart from %s", from)
	}
	if tk, _ := s.Snapshot().Ticket("T2"); tk.Status != TicketTodo {
		t.Fatalf("T2 is %s after restart", tk.Status)
	}
	if tk, _ := s.Snapshot().Ticket("T3"); tk.Status != TicketFailed {
		t.Fatalf("reviewer failed T3 is %s after restart", tk.Status)
	}
	if again, err := recoverSprints(app); err != nil || again != 0 {
		t.Fatalf("recovered the same sprint again: %d %v", again, err)
	}
	id, err := nextID(app)
	if err != nil || id != "sprint-10" {
		t.Fatalf("next id %q %v", id, err)
	}
	if sprintNumber("sprint-x") != 0 || sprintNumber("other-4") != 0 || sprintNumber("sprint-12") != 12 {
		t.Fatal("sprint numbers misread")
	}
}

// Member of a recovered sprint comes back on its stored session when the sprint runs again
func TestMemberReopensStoredSession(t *testing.T) {
	app := testApp(t)
	ctx := context.Background()
	stored := session.New()
	stored.Label(session.ProjectLabel, app.Root)
	stored.Label("kind", KindName(RoleBuilder))
	stored.SetTitle("Builder 1 of sprint-4")
	if err := app.Store.Save(ctx, stored); err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	for _, k := range cfg.Kinds(app.Primary.Profile.Name) {
		if err := app.PutKind(k); err != nil {
			t.Fatal(err)
		}
	}
	s := newSprint("sprint-4", "x", time.Now(), nil)
	s.putMember(&Member{Role: RoleBuilder, Name: "Builder 1", Index: 0, Session: stored.ID()})
	s.putMember(&Member{Role: RoleDirector, Name: "Director", Session: "20260101-000000-0000000000000000"})
	register(s)
	unregister(t, s)
	r := &runner{cfg: cfg, f: &harness.Flow{ID: s.ID, Name: Name, App: app}, s: s}
	m, err := r.member(ctx, RoleBuilder, 0)
	if err != nil {
		t.Fatal(err)
	}
	if m.Live == nil || m.Live.Session.ID() != stored.ID() || m.Live.Session.Labels()[sprintLabel] != s.ID || m.Live.Session.Labels()[roleLabel] != string(RoleBuilder) {
		t.Fatalf("member not on its stored session: %+v", m)
	}
	if again, err := r.member(ctx, RoleBuilder, 0); err != nil || again.Live != m.Live {
		t.Fatalf("second call opened another session: %v %v", again, err)
	}
	d, err := r.member(ctx, RoleDirector, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Live == nil || d.Live.Session.ID() == "20260101-000000-0000000000000000" || d.Session != d.Live.Session.ID() {
		t.Fatalf("director with a lost session got %+v", d)
	}
	var noted bool
	for _, msg := range s.Snapshot().Messages {
		if msg.Kind == MsgNote && strings.Contains(msg.Text, "could not be opened") {
			noted = true
		}
	}
	if !noted {
		t.Fatal("the lost session was not told in the log")
	}
	if _, _, ok := sprintOfSession(stored.ID()); !ok {
		t.Fatal("the reopened session is not known as a member")
	}
}
