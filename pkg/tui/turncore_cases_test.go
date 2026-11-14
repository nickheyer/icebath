package tui

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTurnStoreLiveProgressHelpers(t *testing.T) {
	t.Run("archives completed todos into a transcript trail and clears the live anchor", func(t *testing.T) {
		s := newTUIStore()
		todos := []todoItem{{Content: "prep", ID: "prep", Status: "completed"}, {Content: "serve", ID: "serve", Status: "completed"}}
		s.turn.Todos = todos
		want := []msg{{Kind: "trail", Role: "system", TodoCollapsedByDefault: true, Todos: todos}}
		if got := s.archiveTodosAtTurnEnd(); !reflect.DeepEqual(got, want) || len(s.turn.Todos) != 0 {
			t.Fatalf("got %#v todos %#v", got, s.turn.Todos)
		}
	})
	t.Run("archives incomplete todos with an incomplete flag so the hint renders", func(t *testing.T) {
		s := newTUIStore()
		s.turn.Todos = []todoItem{{Content: "cook", ID: "cook", Status: "completed"}, {Content: "serve", ID: "serve", Status: "in_progress"}, {Content: "eat", ID: "eat", Status: "pending"}}
		archived := s.archiveTodosAtTurnEnd()
		if len(archived) != 1 || !archived[0].TodoIncomplete || len(s.turn.Todos) != 0 {
			t.Fatalf("archived %#v", archived)
		}
		var ids []string
		for _, todo := range archived[0].Todos {
			ids = append(ids, todo.ID)
		}
		if !reflect.DeepEqual(ids, []string{"cook", "serve", "eat"}) {
			t.Fatalf("ids %v", ids)
		}
	})
	t.Run("returns nothing when there are no todos at turn end", func(t *testing.T) {
		if got := newTUIStore().archiveTodosAtTurnEnd(); len(got) != 0 {
			t.Fatalf("got %#v", got)
		}
	})
}

func TestTurnControllerRecordTodosPreservesTheParentField(t *testing.T) {
	newCtl := func() *turnController {
		c := newTurnController(newTUIStore())
		c.fullReset()
		return c
	}
	t.Run("keeps parent on a valid nested subtask", func(t *testing.T) {
		c := newCtl()
		c.recordTodos([]any{
			map[string]any{"content": "Ship feature", "id": "wp1", "status": "in_progress"},
			map[string]any{"content": "Write tests", "id": "t1", "parent": "wp1", "status": "pending"},
		})
		want := []todoItem{{Content: "Ship feature", ID: "wp1", Status: "in_progress"}, {Content: "Write tests", ID: "t1", Parent: "wp1", Status: "pending"}}
		if !reflect.DeepEqual(c.store.turn.Todos, want) {
			t.Fatalf("todos %#v", c.store.turn.Todos)
		}
	})
	t.Run("drops a self-referential parent instead of keeping a self-loop", func(t *testing.T) {
		c := newCtl()
		c.recordTodos([]any{map[string]any{"content": "x", "id": "a", "parent": "a", "status": "pending"}})
		if !reflect.DeepEqual(c.store.turn.Todos, []todoItem{{Content: "x", ID: "a", Status: "pending"}}) {
			t.Fatalf("todos %#v", c.store.turn.Todos)
		}
	})
	t.Run("omits parent entirely when absent, matching pre-nesting payloads", func(t *testing.T) {
		c := newCtl()
		c.recordTodos([]any{map[string]any{"content": "x", "id": "a", "status": "pending"}})
		if !reflect.DeepEqual(c.store.turn.Todos, []todoItem{{Content: "x", ID: "a", Status: "pending"}}) {
			t.Fatalf("todos %#v", c.store.turn.Todos)
		}
	})
}

func TestTurnControllerStartMessageFlashAndYieldNotices(t *testing.T) {
	newCtl := func(n notice) *turnController {
		c := newTurnController(newTUIStore())
		c.fullReset()
		c.store.ui.Notice = &n
		return c
	}
	t.Run("clears a standing credits.usage notice when a new turn starts", func(t *testing.T) {
		c := newCtl(notice{Key: "credits.usage", Kind: "sticky", Level: "warn", Text: "⚠ Credits 90% used · $20.00 cap"})
		c.startMessage()
		if c.store.ui.Notice != nil {
			t.Fatalf("notice %#v", c.store.ui.Notice)
		}
	})
	t.Run("clears a standing credits.grant_spent notice when a new turn starts", func(t *testing.T) {
		c := newCtl(notice{Key: "credits.grant_spent", Kind: "sticky", Level: "info", Text: "• Grant spent · $990.00 top-up left"})
		c.startMessage()
		if c.store.ui.Notice != nil {
			t.Fatalf("notice %#v", c.store.ui.Notice)
		}
	})
	t.Run("leaves a sticky credits.depleted notice across a new turn", func(t *testing.T) {
		c := newCtl(notice{Key: "credits.depleted", Kind: "sticky", Level: "error", Text: "✕ Credit access paused · run /topup to top up"})
		c.startMessage()
		if c.store.ui.Notice == nil || c.store.ui.Notice.Key != "credits.depleted" {
			t.Fatalf("notice %#v", c.store.ui.Notice)
		}
	})
}

func TestSpawnHistoryStoreStatusNormalization(t *testing.T) {
	t.Run("keeps timeout/error statuses from disk snapshots", func(t *testing.T) {
		s := newTUIStore()
		s.pushDiskSnapshot(map[string]any{"finished_at": float64(1_700_000_001), "label": "status test", "session_id": "sess-1", "started_at": float64(1_700_000_000),
			"subagents": []any{
				map[string]any{"goal": "timeout child", "id": "sa-timeout", "index": float64(0), "status": "timeout"},
				map[string]any{"goal": "error child", "id": "sa-error", "index": float64(1), "status": "error"},
			}}, "/tmp/snap-timeout-error.json")
		var statuses []string
		for _, sa := range s.spawnHistory[0].Subagents {
			statuses = append(statuses, sa.Status)
		}
		if !reflect.DeepEqual(statuses, []string{"timeout", "error"}) || s.spawnHistory[0].StartedAt != 1_700_000_000_000 {
			t.Fatalf("statuses %v snapshot %#v", statuses, s.spawnHistory[0])
		}
	})
	t.Run("falls back unknown disk statuses to completed", func(t *testing.T) {
		s := newTUIStore()
		s.pushDiskSnapshot(map[string]any{"finished_at": float64(1_700_000_011), "label": "unknown status test", "session_id": "sess-2", "started_at": float64(1_700_000_010),
			"subagents": []any{map[string]any{"goal": "mystery child", "id": "sa-unknown", "index": float64(0), "status": "mystery_status"}}}, "/tmp/snap-unknown.json")
		if got := s.spawnHistory[0].Subagents[0].Status; got != "completed" {
			t.Fatalf("status %q", got)
		}
	})
}

// Controller whose store clock stands still
func stillController() *turnController {
	s := newTUIStore()
	at := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return at }
	return newTurnController(s)
}

// Fires every timer the controller armed, the way the program runs its tick commands
func fireAll(c *turnController) {
	for kind := turnTimer(0); kind < turnTimerCount; kind++ {
		if c.armed[kind] {
			c.fire(turnTimerMsg{kind: kind, gen: c.gen[kind]})
		}
	}
	c.takeCmds()
}

type recordingGateway struct {
	canon  bool
	calls  []map[string]any
	answer map[string]any
}

func (g *recordingGateway) request(_ context.Context, method string, params map[string]any, _ time.Duration) (any, error) {
	g.calls = append(g.calls, map[string]any{"method": method, "params": params})
	return g.answer, nil
}

func (g *recordingGateway) canonical() bool { return g.canon }

func TestTurnControllerTurnFlow(t *testing.T) {
	t.Run("streams deltas on the batch tick and seals the final reply with its reasoning", func(t *testing.T) {
		c := stillController()
		c.store.ui.ShowReasoning = true
		c.startMessage()
		c.recordReasoningDelta("weighing options", false)
		c.recordMessageDelta("Hello ")
		if c.store.turn.Streaming != "" || !c.armed[timerStream] {
			t.Fatal("stream published before its tick")
		}
		c.recordMessageDelta("world")
		fireAll(c)
		if c.store.turn.Streaming != "Hello world" {
			t.Fatalf("streaming %q", c.store.turn.Streaming)
		}
		result := c.recordMessageComplete(map[string]any{"text": "Hello world"})
		last := result.FinalMessages[len(result.FinalMessages)-1]
		if result.FinalText != "Hello world" || !reflect.DeepEqual(last, msg{Role: "assistant", Text: "Hello world"}) || c.store.ui.Busy {
			t.Fatalf("result %#v busy %v", result, c.store.ui.Busy)
		}
		if !strings.Contains(result.FinalMessages[0].Thinking, "weighing options") {
			t.Fatalf("reasoning segment %#v", result.FinalMessages)
		}
	})
	t.Run("turns a tool call into a finished trail row on the reply's shelf", func(t *testing.T) {
		c := stillController()
		c.startMessage()
		c.recordToolStart("t1", "read_file", "main.go", "", nil)
		if len(c.store.turn.Tools) != 1 {
			t.Fatalf("tools %#v", c.store.turn.Tools)
		}
		took := 0.94
		c.recordToolComplete("t1", "", "", &took, nil, "", nil)
		if !reflect.DeepEqual(c.store.turn.StreamPendingTools, []string{`Read File("main.go") (0.9s) ✓`}) || !reflect.DeepEqual(c.store.turn.TurnTrail, []string{analyzingToolOutput}) {
			t.Fatalf("pending %#v trail %#v", c.store.turn.StreamPendingTools, c.store.turn.TurnTrail)
		}
		result := c.recordMessageComplete(map[string]any{"text": "done"})
		if len(result.FinalMessages) != 2 || !reflect.DeepEqual(result.FinalMessages[0].Tools, []string{`Read File("main.go") (0.9s) ✓`}) {
			t.Fatalf("final %#v", result.FinalMessages)
		}
	})
	t.Run("an interrupt seals the partial reply and cools down to ready", func(t *testing.T) {
		c := stillController()
		c.startMessage()
		c.recordMessageDelta("partial answer")
		var appended []msg
		gw := &recordingGateway{}
		cmd, err := c.interruptTurn(interruptDeps{AppendMessage: func(m msg) { appended = append(appended, m) }, Gateway: gw, Sid: "s1", Sys: func(string) {}}, false)
		if err != nil || cmd == nil {
			t.Fatalf("interrupt %v", err)
		}
		cmd()
		if len(gw.calls) != 1 || gw.calls[0]["method"] != "session.interrupt" {
			t.Fatalf("calls %#v", gw.calls)
		}
		if len(appended) != 1 || appended[0].Text != "partial answer\n\n*[interrupted]*" || c.store.ui.Status != "interrupted" {
			t.Fatalf("appended %#v status %q", appended, c.store.ui.Status)
		}
		fireAll(c)
		if c.store.ui.Status != "ready" {
			t.Fatalf("status %q", c.store.ui.Status)
		}
		reply := c.recordMessageComplete(map[string]any{"status": "interrupted", "text": "partial answer and more"})
		if !reply.WasInterrupted || reply.InterruptedReply == nil || reply.InterruptedReply.To != "partial answer and more\n\n*[interrupted]*" {
			t.Fatalf("reply %#v", reply)
		}
	})
	t.Run("a canonical interrupt seals only when the owner confirms the running generation", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		c := stillController()
		c.store.ui.Sid = "s1"
		c.store.ui.Info = map[string]any{"execution_epoch": "3", "execution_generation": float64(7)}
		c.startMessage()
		c.recordMessageDelta("x")
		gw := &recordingGateway{canon: true, answer: map[string]any{"execution_state": "running", "execution_generation": float64(7)}}
		var appended []msg
		cmd, err := c.interruptTurn(interruptDeps{AppendMessage: func(m msg) { appended = append(appended, m) }, Gateway: gw, Sid: "s1", Sys: func(string) {}}, true)
		if err != nil || len(appended) != 0 {
			t.Fatalf("sealed before the answer %v %#v", err, appended)
		}
		c.settleInterrupt(cmd().(interruptReplyMsg))
		if len(appended) != 1 || !c.store.ui.Busy || c.store.ui.Status != tr("session.status.interrupting") {
			t.Fatalf("appended %#v busy %v status %q", appended, c.store.ui.Busy, c.store.ui.Status)
		}
		if gw.calls[0]["params"].(map[string]any)["execution_generation"] != float64(7) {
			t.Fatalf("params %#v", gw.calls[0])
		}
	})
	t.Run("a ttl notice held while busy shows at turn end and expires", func(t *testing.T) {
		c := stillController()
		c.startMessage()
		ttl := float64(8000)
		c.showNotice(notice{Key: "credits.restored", Kind: "ttl", Text: "restored", TTLms: &ttl})
		if c.store.ui.Notice != nil {
			t.Fatal("notice shown while busy")
		}
		c.recordMessageComplete(map[string]any{"text": "ok"})
		if c.store.ui.Notice == nil || c.store.ui.Notice.ID != "n1" {
			t.Fatalf("notice %#v", c.store.ui.Notice)
		}
		fireAll(c)
		if c.store.ui.Notice != nil {
			t.Fatal("ttl notice outlived its clock")
		}
	})
	t.Run("subagents keep spawn order and are archived when the turn completes", func(t *testing.T) {
		c := stillController()
		c.store.ui.Sid = "s1"
		c.startMessage()
		c.upsertSubagent(map[string]any{"subagent_id": "b", "goal": "second", "task_index": float64(1), "depth": float64(0)}, nil, true)
		c.upsertSubagent(map[string]any{"subagent_id": "a", "goal": "first", "task_index": float64(0), "depth": float64(0)}, nil, true)
		c.upsertSubagent(map[string]any{"subagent_id": "a", "tool_count": float64(3)}, func(_ subagentProgress, next *subagentProgress) { next.Status = "completed" }, true)
		c.upsertSubagent(map[string]any{"subagent_id": "late"}, nil, false)
		got := c.store.turn.Subagents
		if len(got) != 2 || got[0].ID != "a" || got[0].ToolCount != 3 || got[0].Status != "completed" || got[1].ID != "b" {
			t.Fatalf("subagents %#v", got)
		}
		result := c.recordMessageComplete(map[string]any{"text": "ok"})
		if len(result.FinishedSubagents) != 2 || result.SessionID != "s1" || len(c.store.spawnHistory) != 1 || c.store.spawnHistory[0].Label != "first · second" {
			t.Fatalf("result %#v history %#v", result, c.store.spawnHistory)
		}
	})
}
