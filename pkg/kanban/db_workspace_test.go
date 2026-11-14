package kanban

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/state"
	"golang.org/x/text/unicode/norm"
)

// Runs git in a repo failing the test on a non zero exit
func gitRun(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// Initializes a committed git repo at the path like the hermes test helper
func initGitRepo(t *testing.T, repo string) {
	t.Helper()
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-b", "main", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	gitRun(t, repo, "config", "user.email", "kanban@example.com")
	gitRun(t, repo, "config", "user.name", "Kanban Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "README.md")
	gitRun(t, repo, "commit", "-m", "init")
}

// Resolved workspace of a task persisted on its row
func materializeWorkspace(t *testing.T, conn state.SQLiteConn, tid string) string {
	t.Helper()
	ctx := context.Background()
	ws, err := ResolveWorkspace(ctx, mustTask(t, conn, tid), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetWorkspacePath(ctx, conn, tid, ws); err != nil {
		t.Fatal(err)
	}
	return ws
}

// Attachment names and stored paths of a task
func attachmentPairs(t *testing.T, conn state.SQLiteConn, tid string) [][2]string {
	t.Helper()
	atts, err := ListAttachments(context.Background(), conn, tid)
	if err != nil {
		t.Fatal(err)
	}
	var out [][2]string
	for _, a := range atts {
		out = append(out, [2]string{a.Filename, a.StoredPath})
	}
	return out
}

// String list of a decoded JSON list
func stringList(v any) []string {
	var out []string
	if list, ok := v.([]any); ok {
		for _, item := range list {
			out = append(out, cellStr(item))
		}
	}
	return out
}

func TestPathKeyIgnoresUnicodeForm(t *testing.T) {
	composed := "/tmp/repo-Persönlich"
	decomposed := norm.NFD.String(composed)
	if composed == decomposed {
		t.Fatal("forms are the same string")
	}
	if pathKey(composed) != pathKey(decomposed) || pathKey("") != "" || pathKey("/tmp/repo-Personlich") == pathKey(composed) {
		t.Fatal("path key")
	}
}

func TestWorktreeWorkspaceExplicitTargetMaterializesLinkedWorktree(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	initGitRepo(t, repo)
	target := filepath.Join(repo, ".worktrees", "custom-task")
	branch := "wt/custom-task"
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "ship", WorkspaceKind: sp("worktree"), WorkspacePath: sp(target), BranchName: sp(branch)})
	ws, err := ResolveWorkspace(ctx, mustTask(t, conn, tid), nil)
	if err != nil || ws != target || !pathExists(ws) {
		t.Fatal(ws, err)
	}
	repoCommon := strings.TrimSpace(gitRun(t, repo, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	wsCommon := strings.TrimSpace(gitRun(t, ws, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	if wsCommon != repoCommon {
		t.Fatal(wsCommon, repoCommon)
	}
	listed := gitRun(t, repo, "worktree", "list", "--porcelain")
	if !strings.Contains(listed, "worktree "+target) || !strings.Contains(listed, "branch refs/heads/"+branch) {
		t.Fatal(listed)
	}
}

func TestCompleteTaskPersistsScratchArtifactsBeforeCleanup(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "render chart"})
	ws := materializeWorkspace(t, conn, tid)
	artifact := filepath.Join(ws, "chart.png")
	if err := os.WriteFile(artifact, []byte("png-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := pyjson.ObjectOf("artifacts", []any{artifact})
	if ok, err := CompleteTask(ctx, conn, tid, CompleteTaskOptions{Result: sp("ok"), Metadata: meta}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	completed := eventsOfKind(t, conn, tid, "completed")
	arts, _ := payloadOf(completed[len(completed)-1]).Get("artifacts")
	persisted := stringList(arts)[0]
	run, err := LatestRun(ctx, conn, tid)
	if err != nil || run == nil {
		t.Fatal("run", err)
	}
	attDir, _ := TaskAttachmentsDir(ctx, tid, nil)
	data, readErr := os.ReadFile(persisted)
	if pathExists(ws) || readErr != nil || pypath.PathParent(persisted) != attDir || pypath.PathName(persisted) != "chart.png" || string(data) != "png-bytes" || persisted == artifact {
		t.Fatal(ws, persisted, readErr)
	}
	runArts, _ := run.Metadata.(*pyjson.Object).Get("artifacts")
	if got := stringList(runArts); len(got) != 1 || got[0] != persisted {
		t.Fatal(got)
	}
	resolved, _ := pypath.Resolve(persisted)
	if pairs := attachmentPairs(t, conn, tid); len(pairs) != 1 || pairs[0] != [2]string{"chart.png", resolved} {
		t.Fatal(pairs)
	}
}

func TestReviewBoundHandoffPreservesDeclaredArtifacts(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "review bound"})
	ws := materializeWorkspace(t, conn, tid)
	artifact := filepath.Join(ws, "evidence.json")
	if err := os.WriteFile(artifact, []byte(`{"ok": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimTask(ctx, conn, tid, nil, nil); err != nil {
		t.Fatal(err)
	}
	runID := mustTask(t, conn, tid).CurrentRunID
	if runID == nil {
		t.Fatal("no run")
	}
	ok, _, err := RequestReview(ctx, conn, tid, RequestReviewOptions{Summary: sp("ready for review"), Metadata: pyjson.ObjectOf("artifacts", []any{artifact}), ExpectedRunID: runID})
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	handoffs := eventsOfKind(t, conn, tid, "review_requested")
	if ok, err := CompleteTask(ctx, conn, tid, CompleteTaskOptions{Summary: sp("approved")}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	arts, _ := payloadOf(handoffs[len(handoffs)-1]).Get("artifacts")
	persisted := stringList(arts)[0]
	attDir, _ := TaskAttachmentsDir(ctx, tid, nil)
	data, readErr := os.ReadFile(persisted)
	if pathExists(ws) || readErr != nil || pypath.PathParent(persisted) != attDir || string(data) != `{"ok": true}` {
		t.Fatal(ws, persisted, readErr)
	}
	resolved, _ := pypath.Resolve(persisted)
	if pairs := attachmentPairs(t, conn, tid); len(pairs) != 1 || pairs[0] != [2]string{"evidence.json", resolved} {
		t.Fatal(pairs)
	}
}

func TestRequestReviewRollbackDiscardsStagedCopies(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "review rollback"})
	ws := materializeWorkspace(t, conn, tid)
	artifact := filepath.Join(ws, "evidence.json")
	if err := os.WriteFile(artifact, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimTask(ctx, conn, tid, nil, nil); err != nil {
		t.Fatal(err)
	}
	runID := mustTask(t, conn, tid).CurrentRunID
	opts := func() RequestReviewOptions {
		return RequestReviewOptions{Summary: sp("ready"), Metadata: pyjson.ObjectOf("artifacts", []any{artifact}), ExpectedRunID: runID}
	}
	prev := endOrSynthesizeRunFn
	endOrSynthesizeRunFn = func(context.Context, state.SQLiteConn, string, string, string, *string, any, bool, runProfile) (*int64, error) {
		return nil, &RuntimeError{"run bookkeeping failed"}
	}
	_, _, err := RequestReview(ctx, conn, tid, opts())
	endOrSynthesizeRunFn = prev
	var rt *RuntimeError
	if !errors.As(err, &rt) {
		t.Fatal("expected RuntimeError", err)
	}
	attDir, _ := TaskAttachmentsDir(ctx, tid, nil)
	entries, _ := os.ReadDir(attDir)
	if mustTask(t, conn, tid).Status != "running" || len(entries) != 0 {
		t.Fatal("rollback left state", len(entries))
	}
	if ok, _, err := RequestReview(ctx, conn, tid, opts()); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if pairs := attachmentPairs(t, conn, tid); len(pairs) != 1 || pairs[0][0] != "evidence.json" {
		t.Fatal(pairs)
	}
	entries, err = os.ReadDir(attDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "evidence.json" {
		t.Fatal(err, entries)
	}
}

func TestDirChildCompletionUnblocksDeferredScratchParent(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	childDir := filepath.Join(t.TempDir(), "persistent-child")
	if err := os.Mkdir(childDir, 0o755); err != nil {
		t.Fatal(err)
	}
	parent := mustCreate(t, conn, CreateTaskOptions{Title: "scratch parent"})
	child := mustCreate(t, conn, CreateTaskOptions{Title: "dir child", WorkspaceKind: sp("dir"), WorkspacePath: sp(childDir)})
	if _, err := LinkTasks(ctx, conn, parent, child, nil); err != nil {
		t.Fatal(err)
	}
	parentWS := materializeWorkspace(t, conn, parent)
	if _, err := CompleteTask(ctx, conn, parent, CompleteTaskOptions{Result: sp("handoff")}); err != nil {
		t.Fatal(err)
	}
	if !pathExists(parentWS) {
		t.Fatal("parent cleanup not deferred while the dir child is active")
	}
	if _, err := CompleteTask(ctx, conn, child, CompleteTaskOptions{Result: sp("built")}); err != nil {
		t.Fatal(err)
	}
	if pathExists(parentWS) {
		t.Fatal("A 'dir' child completing must trigger the parent scratch sweep")
	}
	if !pathExists(childDir) {
		t.Fatal("Non-scratch 'dir' child workspace is never deleted")
	}
}

func TestIsManagedScratchPathRejectsKanbanMetadataSubtrees(t *testing.T) {
	h := setupKanbanHome(t)
	managed := func(p string) bool {
		ok, err := isManagedScratchPath(p)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	kanbanRoot := filepath.Join(h, "kanban")
	for _, dir := range []string{kanbanRoot, filepath.Join(kanbanRoot, "logs"), filepath.Join(kanbanRoot, "boards", "my-board"),
		filepath.Join(kanbanRoot, "boards", "my-board", "logs"), filepath.Join(kanbanRoot, "boards", "my-board", "workspaces")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if managed(dir) {
			t.Fatal("metadata subtree treated as managed", dir)
		}
	}
	taskDir := filepath.Join(kanbanRoot, "boards", "my-board", "workspaces", "task-42")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !managed(taskDir) {
		t.Fatal("task scratch dir not managed")
	}
}

func TestSymlinkedWorkspacesRootDoesNotWidenScratchCleanup(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	h := KanbanHome()
	broad := filepath.Join(t.TempDir(), "user-data")
	victim := filepath.Join(broad, "project")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("user data"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsRoot := filepath.Join(h, "kanban", "workspaces")
	if st, err := os.Lstat(wsRoot); err == nil && st.IsDir() {
		os.Remove(wsRoot)
	}
	if err := os.MkdirAll(filepath.Dir(wsRoot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(broad, wsRoot); err != nil {
		t.Fatal(err)
	}
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "scratch"})
	if err := SetWorkspacePath(ctx, conn, tid, victim); err != nil {
		t.Fatal(err)
	}
	if ok, err := CompleteTask(ctx, conn, tid, CompleteTaskOptions{Result: sp("done")}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if !isFile(filepath.Join(victim, "keep.txt")) {
		t.Fatal("user data removed through the symlinked root")
	}
}

func TestMaybeEmitScratchTipFiresOncePerInstall(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	wt := mustCreate(t, conn, CreateTaskOptions{Title: "worktree task"})
	t1 := mustCreate(t, conn, CreateTaskOptions{Title: "first scratch"})
	t2 := mustCreate(t, conn, CreateTaskOptions{Title: "second scratch"})
	maybeEmitScratchTip(ctx, conn, wt, sp("worktree"))
	if countKind(eventKinds(t, conn, wt), "tip_scratch_workspace") != 0 {
		t.Fatal("worktree task got the tip")
	}
	maybeEmitScratchTip(ctx, conn, t1, sp("scratch"))
	if countKind(eventKinds(t, conn, t1), "tip_scratch_workspace") != 1 {
		t.Fatal("first scratch task missed the tip")
	}
	maybeEmitScratchTip(ctx, conn, t2, sp("scratch"))
	if countKind(eventKinds(t, conn, t2), "tip_scratch_workspace") != 0 {
		t.Fatal("scratch tip re-fired on the same install")
	}
}

func TestScheduleTaskParksTimeDelayWithoutDispatching(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "delayed recheck", Assignee: sp("ops")})
	if ok, err := ScheduleTask(ctx, conn, tid, sp("run next week"), nil); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if mustTask(t, conn, tid).Status != "scheduled" {
		t.Fatal("not scheduled")
	}
	if claimed, err := ClaimTask(ctx, conn, tid, nil, nil); err != nil || claimed != nil {
		t.Fatal("scheduled task claimed", claimed, err)
	}
	found := false
	for _, e := range eventsOfKind(t, conn, tid, "scheduled") {
		if pyjson.Equal(e.Payload, pyjson.ObjectOf("reason", "run next week")) {
			found = true
		}
	}
	if !found {
		t.Fatal("scheduled event payload missing")
	}
}

// Count of a parameterized query
func countRows(t *testing.T, conn state.SQLiteConn, query string, args ...any) int64 {
	t.Helper()
	row, err := QueryOne(context.Background(), conn, query, args...)
	if err != nil || row == nil {
		t.Fatal(query, err)
	}
	return cellInt(row.Vals[0])
}

func TestDeleteArchivedTaskRemovesRelatedRows(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	parent := mustCreate(t, conn, CreateTaskOptions{Title: "parent"})
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "child", Parents: []string{parent}, Assignee: sp("worker")})
	if _, err := AddComment(ctx, conn, tid, "user", "cleanup me"); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimTask(ctx, conn, tid, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := CompleteTask(ctx, conn, tid, CompleteTaskOptions{Result: sp("done")}); err != nil {
		t.Fatal(err)
	}
	if ok, err := ArchiveTask(ctx, conn, tid, nil); err != nil || !ok {
		t.Fatal(ok, err)
	}
	mustExecArgs(t, conn, "INSERT INTO kanban_notify_subs(task_id, platform, chat_id, thread_id, user_id, created_at, last_event_id) VALUES (?, 'telegram', '123', '', 'u', 0, 0)", tid)
	if ok, err := DeleteArchivedTask(ctx, conn, tid); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if task, err := GetTask(ctx, conn, tid); err != nil || task != nil {
		t.Fatal("task survived", err)
	}
	if countRows(t, conn, "SELECT COUNT(*) FROM task_links WHERE child_id = ? OR parent_id = ?", tid, tid) != 0 ||
		countRows(t, conn, "SELECT COUNT(*) FROM task_comments WHERE task_id = ?", tid) != 0 ||
		countRows(t, conn, "SELECT COUNT(*) FROM task_events WHERE task_id = ?", tid) != 0 ||
		countRows(t, conn, "SELECT COUNT(*) FROM task_runs WHERE task_id = ?", tid) != 0 ||
		countRows(t, conn, "SELECT COUNT(*) FROM kanban_notify_subs WHERE task_id = ?", tid) != 0 {
		t.Fatal("related rows survived")
	}
}

func TestDeleteTaskRemovesTaskAndCascades(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "to-delete", Assignee: sp("alice")})
	for _, body := range []string{"comment", "another"} {
		if _, err := AddComment(ctx, conn, tid, "user", body); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := DeleteTask(ctx, conn, tid); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if task, err := GetTask(ctx, conn, tid); err != nil || task != nil {
		t.Fatal("task survived", err)
	}
	comments, _ := ListComments(ctx, conn, tid)
	events, _ := ListEvents(ctx, conn, tid)
	runs, _ := ListRuns(ctx, conn, tid, true, nil, nil)
	if len(comments) != 0 || len(events) != 0 || len(runs) != 0 {
		t.Fatal("cascade incomplete", len(comments), len(events), len(runs))
	}
}

func TestLinkTasksNoDependencyWaitWhenParentDone(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	parent := mustCreate(t, conn, CreateTaskOptions{Title: "done parent"})
	if _, err := CompleteTask(ctx, conn, parent, CompleteTaskOptions{Result: sp("done")}); err != nil {
		t.Fatal(err)
	}
	child := mustCreate(t, conn, CreateTaskOptions{Title: "follower"})
	gated, err := LinkTasks(ctx, conn, parent, child, nil)
	if err != nil || gated || mustTask(t, conn, child).Status != "ready" || countKind(eventKinds(t, conn, child), "dependency_wait") != 0 {
		t.Fatal(gated, err)
	}
}

func TestCreateTaskWithOpenParentEmitsDependencyWait(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	parent := mustCreate(t, conn, CreateTaskOptions{Title: "blocked parent"})
	if err := BlockTask(ctx, conn, parent, "waiting on files", nil); err != nil {
		t.Fatal(err)
	}
	child := mustCreate(t, conn, CreateTaskOptions{Title: "support card", Parents: []string{parent}})
	if mustTask(t, conn, child).Status != "todo" {
		t.Fatal("child not parked")
	}
	waits := eventsOfKind(t, conn, child, "dependency_wait")
	if len(waits) == 0 {
		t.Fatal("parking behind an open parent must be recorded")
	}
	p := payloadOf(waits[len(waits)-1])
	reason, _ := p.Get("reason")
	par, _ := p.Get("parent")
	if reason != "parent_not_done" || par != parent {
		t.Fatal(reason, par)
	}
}

func TestLinkTasksArchivedParentIsTerminalNoGate(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	parent := mustCreate(t, conn, CreateTaskOptions{Title: "archived parent"})
	if _, err := ArchiveTask(ctx, conn, parent, nil); err != nil {
		t.Fatal(err)
	}
	child := mustCreate(t, conn, CreateTaskOptions{Title: "child"})
	if mustTask(t, conn, child).Status != "ready" {
		t.Fatal("child not ready")
	}
	gated, err := LinkTasks(ctx, conn, parent, child, nil)
	if err != nil || gated || mustTask(t, conn, child).Status != "ready" || countKind(eventKinds(t, conn, child), "dependency_wait") != 0 {
		t.Fatal(gated, err)
	}
}

func TestUnlinkTasksTriggersRecomputeReady(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	a := mustCreate(t, conn, CreateTaskOptions{Title: "parent-done"})
	if _, err := CompleteTask(ctx, conn, a, CompleteTaskOptions{Result: sp("done")}); err != nil {
		t.Fatal(err)
	}
	c := mustCreate(t, conn, CreateTaskOptions{Title: "parent-running"})
	if _, err := ClaimTask(ctx, conn, c, nil, sp("worker:1")); err != nil {
		t.Fatal(err)
	}
	b := mustCreate(t, conn, CreateTaskOptions{Title: "child", Parents: []string{a, c}})
	if mustTask(t, conn, b).Status != "todo" {
		t.Fatal("child not todo")
	}
	removed, err := UnlinkTasks(ctx, conn, c, b)
	if err != nil || !removed {
		t.Fatal(removed, err)
	}
	if mustTask(t, conn, b).Status != "ready" {
		t.Fatal("child should promote to ready immediately after unlink_tasks removes its last blocking dependency")
	}
}

func TestArchiveRunningTaskTerminatesWorker(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "x", Assignee: sp("a")})
	host, _, _ := strings.Cut(claimerID(), ":")
	if _, err := ClaimTask(ctx, conn, tid, nil, sp(host+":worker")); err != nil {
		t.Fatal(err)
	}
	prevProbes := GatewayStatusProbes
	GatewayStatusProbes.ProcessStartTime = func(int) any { return int64(777) }
	GatewayStatusProbes.InstantiationEpoch = func() string { return "boot:1" }
	t.Cleanup(func() { GatewayStatusProbes = prevProbes })
	if err := setWorkerPid(ctx, conn, tid, 54321, nil, nil); err != nil {
		t.Fatal(err)
	}
	stubPidAlive(t, false)
	var signalled []int
	ok, err := ArchiveTask(ctx, conn, tid, func(pid int, _ syscall.Signal) error {
		signalled = append(signalled, pid)
		return nil
	})
	if err != nil || !ok || len(signalled) == 0 || signalled[0] != 54321 {
		t.Fatal(ok, err, signalled)
	}
	row, err := QueryOne(ctx, conn, "SELECT payload FROM task_events WHERE task_id = ? AND kind = 'archive_worker_termination'", tid)
	if err != nil || row == nil {
		t.Fatal("termination event", err)
	}
	p := jsonDict(row.Get("payload"))
	prevPid, _ := p.Get("prev_pid")
	hostLocal, _ := p.Get("host_local")
	attempted, _ := p.Get("termination_attempted")
	terminated, _ := p.Get("terminated")
	if !pyjson.Equal(prevPid, int64(54321)) || hostLocal != true || attempted != true || terminated != true {
		t.Fatal(prevPid, hostLocal, attempted, terminated)
	}
	if mustTask(t, conn, tid).Status != "archived" {
		t.Fatal("not archived")
	}
}

func TestArchiveNonRunningTaskDoesNotAttemptTermination(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "x", Assignee: sp("a")})
	var signalled []int
	ok, err := ArchiveTask(ctx, conn, tid, func(pid int, _ syscall.Signal) error {
		signalled = append(signalled, pid)
		return nil
	})
	if err != nil || !ok || len(signalled) != 0 {
		t.Fatal(ok, err, signalled)
	}
	row, err := QueryOne(ctx, conn, "SELECT 1 FROM task_events WHERE task_id = ? AND kind = 'archive_worker_termination'", tid)
	if err != nil || row != nil {
		t.Fatal("termination event recorded", err)
	}
}
