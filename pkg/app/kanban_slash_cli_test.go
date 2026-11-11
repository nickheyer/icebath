package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/state"
)

// The kanban_home fixture, a fresh home with an initialized board and no kanban pins
func kanbanSlashHome(t *testing.T) context.Context {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(h, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("HOME", tmp)
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "ICEBATH_KANBAN_") {
			t.Setenv(k, "")
			os.Unsetenv(k)
		}
	}
	ctx := context.Background()
	if _, err := kanban.InitDB(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
	return ctx
}

// Runs fn on a board connection, failing the test on any error
func withKanbanConn(t *testing.T, ctx context.Context, fn func(conn state.SQLiteConn)) {
	t.Helper()
	if err := kanban.ConnectClosing(ctx, "", nil, func(conn state.SQLiteConn) error { fn(conn); return nil }); err != nil {
		t.Fatal(err)
	}
}

// Output of one /kanban invocation, failing on a returned error
func slashKanban(t *testing.T, ctx context.Context, rest string) string {
	t.Helper()
	out, err := RunKanbanSlash(ctx, rest)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Decoded JSON of a slash invocation
func slashKanbanJSON(t *testing.T, ctx context.Context, rest string) any {
	t.Helper()
	out := slashKanban(t, ctx, rest)
	v, err := pyjson.Loads(out)
	if err != nil {
		t.Fatalf("%s: %v\n%s", rest, err, out)
	}
	return v
}

// Created task id, failing on error
func mustCreateTask(t *testing.T, ctx context.Context, conn state.SQLiteConn, o kanban.CreateTaskOptions) string {
	t.Helper()
	id, err := kanban.CreateTask(ctx, conn, o)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func strp(s string) *string { return &s }

func TestKanbanListJsonIncludesSessionID(t *testing.T) {
	ctx := kanbanSlashHome(t)
	withKanbanConn(t, ctx, func(conn state.SQLiteConn) {
		mustCreateTask(t, ctx, conn, kanban.CreateTaskOptions{Title: "acp task", Assignee: strp("alice"), SessionID: strp("acp-x")})
	})
	rows, _ := slashKanbanJSON(t, ctx, "list --json").([]any)
	found := false
	for _, row := range rows {
		obj := row.(*pyjson.Object)
		if obj.GetOr("title", nil) == "acp task" && obj.GetOr("session_id", nil) == "acp-x" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no acp task row with its session id in %v", rows)
	}
}

func TestRunSlashStatsTextIncludesReviewLane(t *testing.T) {
	ctx := kanbanSlashHome(t)
	withKanbanConn(t, ctx, func(conn state.SQLiteConn) {
		tid := mustCreateTask(t, ctx, conn, kanban.CreateTaskOptions{Title: "needs review", Assignee: strp("alice")})
		ok, _, err := kanban.RequestReview(ctx, conn, tid, kanban.RequestReviewOptions{Summary: strp("s"), Force: true})
		if err != nil || !ok {
			t.Fatalf("request review: %v %v", ok, err)
		}
	})
	if text := slashKanban(t, ctx, "stats"); !regexp.MustCompile(`review\s+1`).MatchString(text) {
		t.Fatalf("stats text lacks the review lane:\n%s", text)
	}
	stats := slashKanbanJSON(t, ctx, "stats --json").(*pyjson.Object)
	if got := stats.GetOr("by_status", nil).(*pyjson.Object).GetOr("review", nil); !pyjson.Equal(got, 1) {
		t.Fatalf("by_status review = %v", got)
	}
}

func TestKanbanShowJSONIncludesRuntimeLimit(t *testing.T) {
	ctx := kanbanSlashHome(t)
	var bounded, uncapped string
	withKanbanConn(t, ctx, func(conn state.SQLiteConn) {
		limit := int64(2700)
		bounded = mustCreateTask(t, ctx, conn, kanban.CreateTaskOptions{Title: "bounded task", MaxRuntimeSeconds: &limit})
		uncapped = mustCreateTask(t, ctx, conn, kanban.CreateTaskOptions{Title: "uncapped task"})
	})
	b := slashKanbanJSON(t, ctx, "show "+bounded+" --json").(*pyjson.Object).GetOr("task", nil).(*pyjson.Object)
	u := slashKanbanJSON(t, ctx, "show "+uncapped+" --json").(*pyjson.Object).GetOr("task", nil).(*pyjson.Object)
	if !pyjson.Equal(b.GetOr("max_runtime_seconds", nil), 2700) || u.GetOr("max_runtime_seconds", 0) != nil {
		t.Fatalf("bounded %v uncapped %v", b.GetOr("max_runtime_seconds", nil), u.GetOr("max_runtime_seconds", 0))
	}
}

func TestKanbanShowTextRendersGraphWithOpenConnection(t *testing.T) {
	ctx := kanbanSlashHome(t)
	var parent, child string
	withKanbanConn(t, ctx, func(conn state.SQLiteConn) {
		parent = mustCreateTask(t, ctx, conn, kanban.CreateTaskOptions{Title: "parent task"})
		child = mustCreateTask(t, ctx, conn, kanban.CreateTaskOptions{Title: "child task"})
		if _, err := kanban.LinkTasks(ctx, conn, parent, child, nil); err != nil {
			t.Fatal(err)
		}
	})
	out := slashKanban(t, ctx, "show "+child)
	if !strings.Contains(out, "child task") || !strings.Contains(out, parent) || strings.Contains(out, "Cannot operate on a closed database") {
		t.Fatalf("show output:\n%s", out)
	}
}

func TestKanbanShowTextRendersLegacyIsoCompletedAt(t *testing.T) {
	ctx := kanbanSlashHome(t)
	var tid string
	withKanbanConn(t, ctx, func(conn state.SQLiteConn) {
		tid = mustCreateTask(t, ctx, conn, kanban.CreateTaskOptions{Title: "legacy stamp"})
		if _, err := kanban.CompleteTask(ctx, conn, tid, kanban.CompleteTaskOptions{Result: strp("ok"), Force: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(ctx, "UPDATE tasks SET completed_at = '2026-09-30T14:22:11' WHERE id = ?", tid); err != nil {
			t.Fatal(err)
		}
	})
	out := slashKanban(t, ctx, "show "+tid)
	if !strings.Contains(strings.Join(strings.Fields(out), " "), "completed: 2026-09-30 14:22") {
		t.Fatalf("show output:\n%s", out)
	}
}

func TestShowSurvivesIsoStringsInIntegerTimestampColumns(t *testing.T) {
	ctx := kanbanSlashHome(t)
	const isoUTC, isoZ = "2026-08-28T14:35:22+00:00", "2026-08-28T16:58:56.613Z"
	var tid string
	withKanbanConn(t, ctx, func(conn state.SQLiteConn) {
		tid = mustCreateTask(t, ctx, conn, kanban.CreateTaskOptions{Title: "corrupted card", Assignee: strp("alice")})
		if claimed, err := kanban.ClaimTask(ctx, conn, tid, nil, nil); err != nil || claimed == nil {
			t.Fatalf("claim: %v %v", claimed, err)
		}
		if done, err := kanban.CompleteTask(ctx, conn, tid, kanban.CompleteTaskOptions{Summary: strp("done")}); err != nil || !done {
			t.Fatalf("complete: %v %v", done, err)
		}
		for _, q := range []struct {
			sql string
			arg string
		}{{"UPDATE task_runs SET ended_at = ? WHERE task_id = ?", isoUTC}, {"UPDATE tasks SET completed_at = ? WHERE id = ?", isoZ},
			{"UPDATE task_events SET created_at = ? WHERE task_id = ?", isoZ}} {
			if _, err := conn.ExecContext(ctx, q.sql, q.arg, tid); err != nil {
				t.Fatal(err)
			}
		}
	})
	out := slashKanban(t, ctx, "show "+tid)
	if !strings.Contains(out, "Task "+tid+": corrupted card") || strings.Contains(out, "invalid literal") {
		t.Fatalf("show output:\n%s", out)
	}
	payload := slashKanbanJSON(t, ctx, "show "+tid+" --json").(*pyjson.Object)
	if got := payload.GetOr("task", nil).(*pyjson.Object).GetOr("id", nil); got != tid {
		t.Fatalf("task id %v", got)
	}
	runs := payload.GetOr("runs", nil).([]any)
	if len(runs) != 1 || !pyjson.Equal(runs[0].(*pyjson.Object).GetOr("ended_at", nil), 1787927722) {
		t.Fatalf("runs %v", runs)
	}
	events := payload.GetOr("events", nil).([]any)
	if len(events) == 0 {
		t.Fatal("no events")
	}
	for _, e := range events {
		if !pyjson.Equal(e.(*pyjson.Object).GetOr("created_at", nil), 1787936336) {
			t.Fatalf("event created_at %v", e.(*pyjson.Object).GetOr("created_at", nil))
		}
	}
}

func TestKanbanEditUpdatesDocumentedTaskFields(t *testing.T) {
	ctx := kanbanSlashHome(t)
	var tid string
	withKanbanConn(t, ctx, func(conn state.SQLiteConn) {
		tid = mustCreateTask(t, ctx, conn, kanban.CreateTaskOptions{Title: "old title", Body: strp("old body"), Priority: 2})
	})
	slashKanban(t, ctx, "edit "+tid+" --title 'new title' --body 'new body' --priority 70")
	withKanbanConn(t, ctx, func(conn state.SQLiteConn) {
		task, err := kanban.GetTask(ctx, conn, tid)
		if err != nil {
			t.Fatal(err)
		}
		if task.Title != "new title" || task.Body == nil || *task.Body != "new body" || task.Priority != 70 {
			t.Fatalf("task %q %v %d", task.Title, task.Body, task.Priority)
		}
		events, err := kanban.ListEvents(ctx, conn, tid)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.Kind == "reprioritized" {
				return
			}
		}
		t.Fatal("no reprioritized event")
	})
}

func TestRunSlashReclaimRunningTask(t *testing.T) {
	ctx := kanbanSlashHome(t)
	out1 := slashKanban(t, ctx, "create 'stuck worker task' --assignee broken-model")
	m := regexp.MustCompile(`(t_[a-f0-9]+)`).FindStringSubmatch(out1)
	if m == nil {
		t.Fatalf("create output:\n%s", out1)
	}
	tid := m[1]
	raw := make([]byte, 4)
	_, _ = rand.Read(raw)
	lock := hex.EncodeToString(raw)
	now := time.Now().Unix()
	withKanbanConn(t, ctx, func(conn state.SQLiteConn) {
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{"UPDATE tasks SET status='running', claim_lock=?, claim_expires=?, worker_pid=? WHERE id=?", []any{lock, now + 3600, 4242, tid}},
			{"INSERT INTO task_runs (task_id, status, claim_lock, claim_expires, worker_pid, started_at) VALUES (?, 'running', ?, ?, ?, ?)", []any{tid, lock, now + 3600, 4242, now}},
			{"UPDATE tasks SET current_run_id=(SELECT MAX(id) FROM task_runs WHERE task_id=?) WHERE id=?", []any{tid, tid}},
		} {
			if _, err := conn.ExecContext(ctx, q.sql, q.args...); err != nil {
				t.Fatal(err)
			}
		}
	})
	if out := slashKanban(t, ctx, "reclaim "+tid+" --reason 'test'"); !strings.Contains(out, "Reclaimed") {
		t.Fatal(out)
	}
	if out := slashKanban(t, ctx, "show "+tid); !strings.Contains(strings.ToLower(out), "ready") {
		t.Fatal(out)
	}
}
