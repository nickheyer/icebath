package agent

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func loopInterval(n int64) *int64 { return &n }

func TestLoopStateSerdeRoundTrip(t *testing.T) {
	s := NewSessionLoopState("check CI")
	s.Mode = "interval"
	s.IntervalSeconds, s.CurrentDelay = 300.0, 300.0
	s.Times, s.Until, s.TicksFired = 5, "ci is green", 2
	s.Route = pyjson.ObjectOf("platform", "telegram", "chat_id", "123")
	raw, err := s.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	s2, err := SessionLoopStateFromJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Prompt != "check CI" || s2.IntervalSeconds != 300.0 || s2.Times != 5 || s2.Until != "ci is green" || s2.TicksFired != 2 {
		t.Fatalf("restored %#v", s2)
	}
	if !pyrepr.Equal(s2.Route, pyjson.ObjectOf("platform", "telegram", "chat_id", "123")) {
		t.Fatalf("route %s", pyrepr.Repr(s2.Route))
	}
}

func TestLoopStateSerdeOldRowMissingFields(t *testing.T) {
	s, err := SessionLoopStateFromJSON(`{"prompt": "p"}`)
	if err != nil {
		t.Fatal(err)
	}
	if s.Prompt != "p" || s.Status != "active" || s.Route == nil || s.Route.Len() != 0 {
		t.Fatalf("state %#v", s)
	}
}

func TestPersistenceSaveLoadClear(t *testing.T) {
	goalsTestHome(t)
	ctx := context.Background()
	mgr := NewLoopManager(ctx, "sess-1")
	if _, err := mgr.Set(ctx, "check the deploy", loopInterval(300), 0, "", nil); err != nil {
		t.Fatal(err)
	}
	loaded := LoadLoop(ctx, "sess-1")
	if loaded == nil || loaded.Prompt != "check the deploy" || loaded.Status != "active" {
		t.Fatalf("loaded %#v", loaded)
	}
	if !mgr.Clear(ctx) {
		t.Fatal("clear refused")
	}
	if cleared := LoadLoop(ctx, "sess-1"); cleared == nil || cleared.Status != "cleared" {
		t.Fatalf("cleared %#v", cleared)
	}
}

func TestPersistenceListActiveLoops(t *testing.T) {
	goalsTestHome(t)
	ctx := context.Background()
	if _, err := NewLoopManager(ctx, "a").Set(ctx, "task a", loopInterval(60), 0, "", nil); err != nil {
		t.Fatal(err)
	}
	mgrB := NewLoopManager(ctx, "b")
	if _, err := mgrB.Set(ctx, "task b", loopInterval(60), 0, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := mgrB.Pause(ctx, loopDefaultPauseReason); err != nil {
		t.Fatal(err)
	}
	active := map[string]*SessionLoopState{}
	for _, e := range ListActiveLoops(ctx) {
		active[e.SessionID] = e.State
	}
	if _, ok := active["a"]; !ok {
		t.Fatal("active loop a missing")
	}
	if _, ok := active["b"]; ok {
		t.Fatal("paused loop b listed")
	}
}

func TestPersistenceMigrateToSession(t *testing.T) {
	goalsTestHome(t)
	ctx := context.Background()
	if _, err := NewLoopManager(ctx, "parent").Set(ctx, "watch it", loopInterval(60), 0, "", nil); err != nil {
		t.Fatal(err)
	}
	if !migrateLoopToSession(ctx, "parent", "child", "compression") {
		t.Fatal("migration refused")
	}
	if child := LoadLoop(ctx, "child"); child == nil || child.Prompt != "watch it" {
		t.Fatalf("child %#v", child)
	}
	if parent := LoadLoop(ctx, "parent"); parent == nil || parent.Status != "cleared" {
		t.Fatalf("parent %#v", parent)
	}
}

func TestPersistenceMigrateNoSource(t *testing.T) {
	goalsTestHome(t)
	ctx := context.Background()
	if migrateLoopToSession(ctx, "nope", "child2", "") {
		t.Fatal("migrated without a source")
	}
	if migrateLoopToSession(ctx, "same", "same", "") {
		t.Fatal("migrated onto itself")
	}
}

func TestPersistenceMigrateDoesNotClobberChild(t *testing.T) {
	goalsTestHome(t)
	ctx := context.Background()
	if _, err := NewLoopManager(ctx, "p2").Set(ctx, "parent loop", loopInterval(60), 0, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLoopManager(ctx, "c2").Set(ctx, "child loop", loopInterval(60), 0, "", nil); err != nil {
		t.Fatal(err)
	}
	if migrateLoopToSession(ctx, "p2", "c2", "") {
		t.Fatal("migration clobbered the child")
	}
	if got := LoadLoop(ctx, "c2"); got == nil || got.Prompt != "child loop" {
		t.Fatalf("child %#v", got)
	}
}
