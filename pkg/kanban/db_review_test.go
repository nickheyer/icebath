package kanban

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Board at an explicit path under a sandboxed home
func openPathBoard(t *testing.T) (state.SQLiteConn, string) {
	t.Helper()
	setupKanbanHome(t)
	dbPath := filepath.Join(t.TempDir(), "kanban.db")
	conn := mustConnect(t, dbPath, nil)
	t.Cleanup(func() { conn.Close() })
	return conn, dbPath
}

// Done parent with a done child
func doneParentWithDoneChild(t *testing.T, conn state.SQLiteConn) (string, string) {
	t.Helper()
	ctx := context.Background()
	parent := mustCreate(t, conn, CreateTaskOptions{Title: "ancestor", Assignee: sp("planner")})
	if ok, err := CompleteTask(ctx, conn, parent, CompleteTaskOptions{Result: sp("done")}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	child := mustCreate(t, conn, CreateTaskOptions{Title: "child", Assignee: sp("builder"), Parents: []string{parent}})
	if ok, err := CompleteTask(ctx, conn, child, CompleteTaskOptions{Result: sp("done")}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	return parent, child
}

// Flips a done parent back to todo the way a reopen surface does
func reopenParentDirectly(t *testing.T, conn state.SQLiteConn, parent string) {
	t.Helper()
	ctx := context.Background()
	if err := WriteTxn(ctx, conn, false, func() error {
		_, err := conn.ExecContext(ctx, "UPDATE tasks SET status = 'todo', completed_at = NULL WHERE id = ?", parent)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReopenDemotesDoneDescendantsWithEventsAndComments(t *testing.T) {
	conn, _ := openPathBoard(t)
	ctx := context.Background()
	parent, child := doneParentWithDoneChild(t, conn)
	grandchild := mustCreate(t, conn, CreateTaskOptions{Title: "grandchild", Assignee: sp("writer"), Parents: []string{child}})
	if ok, err := CompleteTask(ctx, conn, grandchild, CompleteTaskOptions{Result: sp("done")}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	reopenParentDirectly(t, conn, parent)
	result, err := InvalidateDescendantsForParentReopen(ctx, conn, parent, "operator")
	if err != nil {
		t.Fatal(err)
	}
	demoted := map[string]*pyjson.Object{}
	for _, entry := range result.Invalidated {
		id, _ := entry.Get("id")
		demoted[cellStr(id)] = entry
	}
	if len(demoted) != 2 || demoted[child] == nil || demoted[grandchild] == nil {
		t.Fatal("demoted set", demoted)
	}
	for _, tid := range []string{child, grandchild} {
		prior, _ := demoted[tid].Get("prior_status")
		next, _ := demoted[tid].Get("new_status")
		if prior != "done" || next != "todo" {
			t.Fatal(prior, next)
		}
		task := mustTask(t, conn, tid)
		if task.Status != "todo" || task.CompletedAt != nil {
			t.Fatal(task.Status, task.CompletedAt)
		}
		inval := eventsOfKind(t, conn, tid, "descendant_invalidated")
		if len(inval) != 1 {
			t.Fatal("invalidation events", len(inval))
		}
		p := payloadOf(inval[0])
		anc, _ := p.Get("ancestor")
		ps, _ := p.Get("prior_status")
		ns, _ := p.Get("new_status")
		if anc != parent || ps != "done" || ns != "todo" {
			t.Fatal(p)
		}
		comments, err := ListComments(ctx, conn, tid)
		if err != nil {
			t.Fatal(err)
		}
		named := false
		for _, c := range comments {
			if strings.Contains(c.Body, parent) && c.Author == "operator" {
				named = true
			}
		}
		if !named {
			t.Fatalf("no invalidation comment naming %s on %s", parent, tid)
		}
	}
	if len(result.Terminations) != 0 {
		t.Fatal("terminations", result.Terminations)
	}
}

func TestRunningDescendantEventPrecedesTerminationViaReclaimHelper(t *testing.T) {
	conn, dbPath := openPathBoard(t)
	ctx := context.Background()
	parent := mustCreate(t, conn, CreateTaskOptions{Title: "ancestor", Assignee: sp("planner")})
	if ok, err := CompleteTask(ctx, conn, parent, CompleteTaskOptions{Result: sp("done")}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	child := mustCreate(t, conn, CreateTaskOptions{Title: "running child", Assignee: sp("builder"), Parents: []string{parent}})
	claimed, err := ClaimTask(ctx, conn, child, nil, nil)
	if err != nil || claimed == nil || claimed.Status != "running" {
		t.Fatal("claim", err)
	}
	if err := setWorkerPid(ctx, conn, child, 424242, nil, nil); err != nil {
		t.Fatal(err)
	}
	var kills []WorkerTermination
	prev := terminateReclaimedWorkerFn
	terminateReclaimedWorkerFn = func(pid any, claimLock *string, _ SignalFunc, startedAt any) *pyjson.Object {
		side := mustConnect(t, dbPath, nil)
		kinds := eventKinds(t, side, child)
		side.Close()
		if countKind(kinds, "descendant_invalidated") == 0 {
			t.Error("audit trail not durable before the kill")
		}
		kills = append(kills, WorkerTermination{pid, claimLock, startedAt})
		return payload("terminated", true)
	}
	t.Cleanup(func() { terminateReclaimedWorkerFn = prev })
	reopenParentDirectly(t, conn, parent)
	result, err := InvalidateDescendantsForParentReopen(ctx, conn, parent, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if len(kills) == 0 || cellInt(kills[0].Pid) != 424242 {
		t.Fatal("kills", kills)
	}
	if len(result.Terminations) != len(kills) || cellInt(result.Terminations[0].Pid) != cellInt(kills[0].Pid) || !strPtrEq(result.Terminations[0].ClaimLock, kills[0].ClaimLock) {
		t.Fatal("terminations differ", result.Terminations, kills)
	}
	task := mustTask(t, conn, child)
	if task.Status != "todo" || task.CurrentRunID != nil {
		t.Fatal(task.Status, task.CurrentRunID)
	}
	run, err := LatestRun(ctx, conn, child)
	if err != nil || run == nil || run.Outcome == nil || *run.Outcome != "reclaimed" {
		t.Fatal("run", run, err)
	}
}

func TestCounterResetOnInvalidatedDescendants(t *testing.T) {
	conn, _ := openPathBoard(t)
	ctx := context.Background()
	parent, child := doneParentWithDoneChild(t, conn)
	if err := WriteTxn(ctx, conn, false, func() error {
		_, err := conn.ExecContext(ctx, "UPDATE tasks SET consecutive_failures = 4 WHERE id = ?", child)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reopenParentDirectly(t, conn, parent)
	if _, err := InvalidateDescendantsForParentReopen(ctx, conn, parent, "op"); err != nil {
		t.Fatal(err)
	}
	if mustTask(t, conn, child).ConsecutiveFailures != 0 {
		t.Fatal("counter kept")
	}
}

func TestListCommentsAfterCursor(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "chat"})
	c1, err := AddComment(ctx, conn, tid, "alice", "first")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := AddComment(ctx, conn, tid, "bob", "second")
	if err != nil {
		t.Fatal(err)
	}
	ids := func(after int64) []int64 {
		comments, err := ListCommentsAfter(ctx, conn, tid, after)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, c := range comments {
			out = append(out, c.ID)
		}
		return out
	}
	if got := ids(0); len(got) != 2 || got[0] != c1 || got[1] != c2 {
		t.Fatal(got)
	}
	newer, err := ListCommentsAfter(ctx, conn, tid, c1)
	if err != nil || len(newer) != 1 || newer[0].ID != c2 || newer[0].Body != "second" {
		t.Fatal(newer, err)
	}
	if got := ids(c2); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestListSurvivesUndecodableAndBlobTextCells(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	good := mustCreate(t, conn, CreateTaskOptions{Title: "good card", Assignee: sp("coder")})
	mustExec(t, conn,
		"INSERT INTO tasks (id, title, body, assignee, status, priority, created_by, created_at) "+
			"VALUES ('t_text_bad', 'text invalid', CAST(X'FFFEABCD00' AS TEXT), 'coder', 'ready', 0, 'user', 1000)",
		"INSERT INTO tasks (id, title, body, assignee, status, priority, created_by, created_at) "+
			"VALUES ('t_blob_bad', 'blob body', X'FFFEABCD00', 'coder', 'ready', 0, 'user', 1000)")
	conn.Close()
	conn = mustConnect(t, "", nil)
	defer conn.Close()
	listed, err := ListTasks(ctx, conn, ListTasksOptions{Status: sp("ready")})
	if err != nil {
		t.Fatal(err)
	}
	tasks := map[string]*Task{}
	for _, task := range listed {
		tasks[task.ID] = task
	}
	if tasks[good] == nil || tasks["t_text_bad"] == nil || tasks["t_blob_bad"] == nil {
		t.Fatal("listing dropped cards", len(tasks))
	}
	for _, tid := range []string{"t_text_bad", "t_blob_bad"} {
		if body := tasks[tid].Body; body == nil || !strings.Contains(*body, "�") {
			t.Fatal("body not lossily decoded", tid)
		}
	}
	if mustTask(t, conn, "t_text_bad").Title != "text invalid" {
		t.Fatal("title")
	}
}

func TestShowJsonSurvivesBlobCellsInSiblingTables(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "card", Assignee: sp("coder")})
	mustExecArgs(t, conn, "INSERT INTO task_comments (task_id, author, body, created_at) VALUES (?, 'user', X'FFFEABCD00', 1000)", tid)
	mustExecArgs(t, conn, "INSERT INTO task_events (task_id, kind, payload, created_at) VALUES (?, 'note', X'FFFEABCD00', 1000)", tid)
	conn.Close()
	conn = mustConnect(t, "", nil)
	defer conn.Close()
	comments, err := ListComments(ctx, conn, tid)
	if err != nil || len(comments) == 0 || !strings.Contains(comments[len(comments)-1].Body, "�") {
		t.Fatal("comment body", err)
	}
	events, err := ListEvents(ctx, conn, tid)
	if err != nil || len(events) == 0 {
		t.Fatal("events", err)
	}
	for _, e := range events {
		if _, err := pyjson.Dumps(e.Payload, pyjson.EnsureASCII(false)); err != nil {
			t.Fatal("event payload not serializable", err)
		}
	}
}

func TestReadWorkerLogTail(t *testing.T) {
	h := setupKanbanHome(t)
	ctx := context.Background()
	logDir := filepath.Join(h, "kanban", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, "line "+string(rune('0'+i)))
	}
	if err := os.WriteFile(filepath.Join(logDir, "t_beef.log"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	full, err := ReadWorkerLog(ctx, "t_beef", nil, nil)
	if err != nil || full == nil || !strings.Contains(*full, "line 0") {
		t.Fatal("full log", err)
	}
	tail, err := ReadWorkerLog(ctx, "t_beef", i64ptr(30), nil)
	if err != nil || tail == nil || strings.Contains(*tail, "line 0") {
		t.Fatal("tail", err)
	}
	if missing, err := ReadWorkerLog(ctx, "t_missing", nil, nil); err != nil || missing != nil {
		t.Fatal("missing log", missing, err)
	}
}
