package tui

import (
	"slices"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/vxl/arena"
)

// Newest turn the arena journal holds for the session in view
func arenaTurn(t *testing.T, a *app) *arena.Snapshot {
	t.Helper()
	ids := a.arena.journal.Sprints(a.store.ui.Sid)
	if len(ids) == 0 {
		t.Fatal("the journal holds no turn of the session")
	}
	snap, _, _ := a.arena.journal.View(ids[len(ids)-1], time.Time{})
	return snap
}

// Gateway events of a turn reach the journal as director, crew, tickets, outcome and spend
func TestArenaFeedFollowsTheTurn(t *testing.T) {
	a := testApp(t)
	a.store.ui.Sid = "sid-arena"
	a.setLastUserMsg("split the lexer")
	a.Update(gatewayEventMsg{Type: "message.start", Payload: map[string]any{}})
	snap := arenaTurn(t, a)
	if !snap.Running() || snap.Title != "split the lexer" || snap.Stage != arena.StageBrief {
		t.Fatalf("turn opens as running %v title %q stage %q", snap.Running(), snap.Title, snap.Stage)
	}
	a.Update(gatewayEventMsg{Type: "tool.start", Payload: map[string]any{"tool_id": "t1", "name": "todo_list"}})
	a.Update(gatewayEventMsg{Type: "tool.complete", Payload: map[string]any{"tool_id": "t1", "name": "todo_list", "todos": []any{
		map[string]any{"id": "1", "content": "split the lexer", "status": "in_progress"},
		map[string]any{"id": "2", "content": "scanner", "status": "pending", "parent": "1"},
	}}})
	a.Update(gatewayEventMsg{Type: "subagent.start", Payload: map[string]any{"subagent_id": "a1", "goal": "plan the split", "task_index": 0.0}})
	a.Update(gatewayEventMsg{Type: "subagent.tool", Payload: map[string]any{"subagent_id": "a1", "goal": "plan the split", "tool_name": "read_file", "tool_preview": "lexer.go"}})
	a.Update(gatewayEventMsg{Type: "subagent.start", Payload: map[string]any{"subagent_id": "b1", "parent_id": "a1", "depth": 1.0, "goal": "write the scanner", "task_index": 0.0}})
	a.Update(gatewayEventMsg{Type: "subagent.tool", Payload: map[string]any{"subagent_id": "b1", "goal": "write the scanner", "tool_name": "patch", "tool_preview": "scan.go"}})
	snap = arenaTurn(t, a)
	if snap.Stage != arena.StagePlan {
		t.Fatalf("stage %q while the crew works", snap.Stage)
	}
	ada, _ := snap.Member("Ada")
	linus, _ := snap.Member("Linus")
	if ada.Role != arena.RoleArchitect || ada.ID != "a1" || linus.Role != arena.RoleBuilder || linus.Parent != "Ada" {
		t.Fatalf("crew %+v and %+v", ada, linus)
	}
	if t1, _ := snap.Ticket("T1"); t1.Status != arena.TicketActive || !slices.Equal(t1.DependsOn, []string{"T2"}) {
		t.Fatalf("todo ticket %+v", t1)
	}
	a.Update(gatewayEventMsg{Type: "subagent.complete", Payload: map[string]any{"subagent_id": "b1", "goal": "write the scanner", "status": "completed",
		"summary": "scanner written", "input_tokens": 9000.0, "output_tokens": 1200.0, "cost_usd": 0.02, "files_written": []any{"scan.go"}}})
	a.Update(gatewayEventMsg{Type: "subagent.complete", Payload: map[string]any{"subagent_id": "a1", "goal": "plan the split", "status": "completed", "summary": "split planned"}})
	snap = arenaTurn(t, a)
	if d2, _ := snap.Ticket("D2"); d2.Status != arena.TicketDone {
		t.Fatalf("scanner ticket %q once its architect finished", d2.Status)
	}
	if u := snap.Usage["Linus"]; u.Cost != 0.02 || u.Input != 9000 {
		t.Fatalf("builder spend %+v", u)
	}
	linus, _ = snap.Member("Linus")
	if linus.Summary != "scanner written" || !slices.Equal(linus.FilesWritten, []string{"scan.go"}) {
		t.Fatalf("builder detail %+v", linus)
	}
	a.Update(gatewayEventMsg{Type: "message.complete", Payload: map[string]any{"text": "split done"}})
	snap = arenaTurn(t, a)
	if snap.Running() || snap.Stage != arena.StageReport || snap.Outcome != arena.OutcomeDone {
		t.Fatalf("finished turn running %v stage %q outcome %q", snap.Running(), snap.Stage, snap.Outcome)
	}
	if d1, _ := snap.Ticket("D1"); d1.Status != arena.TicketDone {
		t.Fatalf("architect ticket %q after the final answer", d1.Status)
	}
	kinds := map[arena.MsgKind]int{}
	for _, m := range snap.Messages {
		kinds[m.Kind]++
	}
	if kinds[arena.MsgAssign] != 2 || kinds[arena.MsgHandoff] != 2 || kinds[arena.MsgVerdict] != 2 {
		t.Fatalf("message kinds %v", kinds)
	}
}

// The arena command opens one zoomed card that takes the display settings
func TestArenaCommandOpensTheCard(t *testing.T) {
	a := testApp(t)
	a.store.ui.Arena = map[string]any{"world": "den", "display": "glyphs", "fps": 5.0}
	drain(a, a.dispatchSubmission("/arena"))
	card, ws := a.wm.Find(arena.Name)
	if card == nil || ws.Zoomed() != card || a.wm.Focused() != card {
		t.Fatalf("arena card %v zoomed %v focused %v", card, ws.Zoomed(), a.wm.Focused())
	}
	if card.Title() != "ARENA" || a.arena.cfg.Arena.World != "den" || a.arena.cfg.Arena.FPS != 5 || a.arena.cfgErr != nil {
		t.Fatalf("title %q config %+v error %v", card.Title(), a.arena.cfg.Arena, a.arena.cfgErr)
	}
	drain(a, a.dispatchSubmission("/arena"))
	if again, _ := a.wm.Find(arena.Name); again != card || ws.Zoomed() != card {
		t.Fatal("a second /arena opened another card or left the zoom")
	}
	a.store.ui.Arena = map[string]any{"world": "moon"}
	a.Update(struct{}{})
	if a.arena.cfgErr == nil || a.arena.cfg.Arena.World != "blackwall" {
		t.Fatalf("unknown world parsed as %q with error %v", a.arena.cfg.Arena.World, a.arena.cfgErr)
	}
	drain(a, a.wm.Close(card))
	if a.wm.Has(arena.Name) {
		t.Fatal("the arena stayed open after closing")
	}
	drain(a, a.dispatchSubmission("/arena"))
	if fresh, _ := a.wm.Find(arena.Name); fresh == nil || fresh == card {
		t.Fatalf("reopening after close gave %v", fresh)
	}
}
