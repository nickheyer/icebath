package kanban

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Agent recording every operator note steered into it
type fakeSteerAgent struct{ steers []string }

func (a *fakeSteerAgent) Steer(text string) bool {
	a.steers = append(a.steers, text)
	return true
}

// Isolated home for the comment bridge with its poll state reset
func commentWorkerHome(t *testing.T) {
	t.Helper()
	clearKanbanEnv(t)
	tmp := t.TempDir()
	h := filepath.Join(tmp, "icebath_home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Cleanup(home.PatchUserHome(tmp))
	for _, name := range []string{"ICEBATH_KANBAN_DB", "ICEBATH_KANBAN_WORKSPACES_ROOT", "ICEBATH_KANBAN_HOME", "ICEBATH_KANBAN_BOARD"} {
		unsetEnv(t, name)
	}
	discardInitialized(t, mustDBPath(t, BoardArg("default")))
	bridgeMu.Lock()
	commentWatermark = map[string]int64{}
	commentPollLastAttempt = 0
	bridgeMu.Unlock()
}

// Bypasses the inter poll rate limit of the comment bridge
func unthrottleComments() {
	bridgeMu.Lock()
	commentPollLastAttempt = 0
	bridgeMu.Unlock()
}

// Adds a comment on the default board
func addBoardComment(t *testing.T, tid, author, body string) {
	t.Helper()
	conn := mustConnect(t, "", nil)
	defer conn.Close()
	if _, err := AddComment(context.Background(), conn, tid, author, body); err != nil {
		t.Fatal(err)
	}
}

func TestNoopWithoutWorkerEnv(t *testing.T) {
	commentWorkerHome(t)
	unsetEnv(t, "ICEBATH_KANBAN_TASK")
	agent := &fakeSteerAgent{}
	if InjectNewCommentsFromEnv(context.Background(), agent) || len(agent.steers) != 0 {
		t.Fatal(agent.steers)
	}
}

func TestSeedThenInjectNewComment(t *testing.T) {
	commentWorkerHome(t)
	tid := mustCreateTask(t, CreateTaskOptions{Title: "live task"})
	addBoardComment(t, tid, "desktop", "pre-existing note")
	t.Setenv("ICEBATH_KANBAN_TASK", tid)
	t.Setenv("ICEBATH_PROFILE", "worker-bot")
	agent := &fakeSteerAgent{}
	ctx := context.Background()
	unthrottleComments()
	if InjectNewCommentsFromEnv(ctx, agent) || len(agent.steers) != 0 {
		t.Fatal("seeding poll injected", agent.steers)
	}
	addBoardComment(t, tid, "desktop", "actually use the v2 API")
	unthrottleComments()
	if !InjectNewCommentsFromEnv(ctx, agent) || len(agent.steers) != 1 || !strings.Contains(agent.steers[0], "v2 API") {
		t.Fatal(agent.steers)
	}
	unthrottleComments()
	if InjectNewCommentsFromEnv(ctx, agent) || len(agent.steers) != 1 {
		t.Fatal(agent.steers)
	}
}

func TestSkipsOwnAuthoredComments(t *testing.T) {
	commentWorkerHome(t)
	tid := mustCreateTask(t, CreateTaskOptions{Title: "echo guard"})
	t.Setenv("ICEBATH_KANBAN_TASK", tid)
	t.Setenv("ICEBATH_PROFILE", "worker-bot")
	agent := &fakeSteerAgent{}
	ctx := context.Background()
	unthrottleComments()
	InjectNewCommentsFromEnv(ctx, agent)
	addBoardComment(t, tid, "worker-bot", "i did a thing")
	unthrottleComments()
	if InjectNewCommentsFromEnv(ctx, agent) || len(agent.steers) != 0 {
		t.Fatal(agent.steers)
	}
}

func TestDelegatedChildInWorkerProcessNeitherReceivesNorConsumesNotes(t *testing.T) {
	commentWorkerHome(t)
	tid := mustCreateTask(t, CreateTaskOptions{Title: "live task"})
	t.Setenv("ICEBATH_KANBAN_TASK", tid)
	t.Setenv("ICEBATH_PROFILE", "worker-bot")
	worker := &fakeSteerAgent{}
	ctx := context.Background()
	unthrottleComments()
	if InjectNewCommentsFromEnv(ctx, worker) {
		t.Fatal("seeding poll injected")
	}
	addBoardComment(t, tid, "desktop", "operator note for the worker")
	child := &fakeSteerAgent{}
	unthrottleComments()
	if InjectNewCommentsFromEnv(tools.WithDelegatedChildContext(ctx, true), child) || len(child.steers) != 0 {
		t.Fatal(child.steers)
	}
	unthrottleComments()
	if !InjectNewCommentsFromEnv(ctx, worker) || !strings.Contains(worker.steers[0], "operator note") {
		t.Fatal(worker.steers)
	}
}

func TestSkipsOwnAuthoredCommentsWithoutEnvProfile(t *testing.T) {
	commentWorkerHome(t)
	tid := mustCreateTask(t, CreateTaskOptions{Title: "identity echo guard"})
	t.Setenv("ICEBATH_KANBAN_TASK", tid)
	unsetEnv(t, "ICEBATH_PROFILE")
	unsetEnv(t, "ICEBATH_PROFILE_NAME")
	agent := &fakeSteerAgent{}
	ctx := context.Background()
	unthrottleComments()
	InjectNewCommentsFromEnv(ctx, agent)
	identity := persistedIdentity(ctx)
	if identity == "worker" {
		t.Fatal("identity fell back to the generic label")
	}
	addBoardComment(t, tid, identity, "note to self")
	unthrottleComments()
	if InjectNewCommentsFromEnv(ctx, agent) || len(agent.steers) != 0 {
		t.Fatal(agent.steers)
	}
}

// Claimed scratch task of a parent worker with the run id env set from the claim result as hermes does
func runningKanbanTask(t *testing.T) (tid, workspace, attachmentsRoot string) {
	t.Helper()
	clearKanbanEnv(t)
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	attachmentsRoot = filepath.Join(tmp, "attachments")
	workspace = filepath.Join(tmp, "parent-workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	unsetEnv(t, "ICEBATH_KANBAN_HOME")
	t.Setenv("ICEBATH_PROFILE", "parent-worker")
	t.Setenv("ICEBATH_KANBAN_WORKSPACE", workspace)
	t.Setenv("ICEBATH_KANBAN_ATTACHMENTS_ROOT", attachmentsRoot)
	discardInitialized(t, mustDBPath(t, BoardArg("default")))
	ctx := context.Background()
	if _, err := InitDB(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
	conn := mustConnect(t, "", nil)
	defer conn.Close()
	tid, err := CreateTask(ctx, conn, CreateTaskOptions{Title: "parent", Assignee: strp("parent-worker"), WorkspaceKind: strp("scratch"), WorkspacePath: &workspace})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := ClaimTask(ctx, conn, tid, nil, nil)
	if err != nil || claim == nil {
		t.Fatalf("claim %v %v", claim, err)
	}
	t.Setenv("ICEBATH_KANBAN_TASK", tid)
	t.Setenv("ICEBATH_KANBAN_RUN_ID", claim.ID)
	return tid, workspace, attachmentsRoot
}

// Log handler keeping the message of every record at or above warning
type warningRecorder struct {
	mu       sync.Mutex
	messages []string
}

func (w *warningRecorder) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}
func (w *warningRecorder) Handle(_ context.Context, r slog.Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.messages = append(w.messages, r.Message)
	return nil
}
func (w *warningRecorder) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *warningRecorder) WithGroup(string) slog.Handler      { return w }

// Records the warnings of the kanban tools logger for one test
func captureKanbanToolsWarnings(t *testing.T) *warningRecorder {
	t.Helper()
	rec := &warningRecorder{}
	prev := kanbanToolsLog
	t.Cleanup(func() { kanbanToolsLog = prev })
	kanbanToolsLog = slog.New(rec)
	return rec
}

// Sets the auto heartbeat window and fence warning state
func resetAutoHeartbeat(last float64, warned bool) {
	bridgeMu.Lock()
	autoHeartbeatLastAttempt, autoHeartbeatFenceWarned = last, warned
	bridgeMu.Unlock()
}

func TestAutoHeartbeatReportsFailureWithoutMutatingFencedChildBoard(t *testing.T) {
	tid, _, _ := runningKanbanTask(t)
	t.Cleanup(func() { resetAutoHeartbeat(0, false) })
	before := boardTask(t, tid)
	eventsBefore := boardEvents(t, tid)
	t.Setenv("ICEBATH_DELEGATED_CHILD_CONTEXT", os.Getenv("ICEBATH_HOME"))
	resetAutoHeartbeat(0, false)
	rec := captureKanbanToolsWarnings(t)
	ctx := context.Background()
	if HeartbeatCurrentWorkerFromEnv(ctx) {
		t.Fatal("fenced heartbeat reported success")
	}
	resetAutoHeartbeat(0, autoHeartbeatFenceWarned)
	if HeartbeatCurrentWorkerFromEnv(ctx) {
		t.Fatal("fenced heartbeat reported success")
	}
	var fence []string
	for _, m := range rec.messages {
		if strings.Contains(m, "ICEBATH_DELEGATED_CHILD_CONTEXT") {
			fence = append(fence, m)
		}
	}
	if len(fence) != 1 || !strings.Contains(fence[0], tid) {
		t.Fatal(rec.messages)
	}
	unsetEnv(t, "ICEBATH_DELEGATED_CHILD_CONTEXT")
	resetAutoHeartbeat(0, autoHeartbeatFenceWarned)
	if HeartbeatCurrentWorkerFromEnv(tools.WithDelegatedChildContext(ctx, true)) {
		t.Fatal("delegate child heartbeat reported success")
	}
	bridgeMu.Lock()
	last := autoHeartbeatLastAttempt
	bridgeMu.Unlock()
	if last != 0 {
		t.Fatal("delegate child consumed the worker heartbeat window", last)
	}
	after := boardTask(t, tid)
	if !reflect.DeepEqual(after.ClaimExpires, before.ClaimExpires) || !reflect.DeepEqual(after.LastHeartbeatAt, before.LastHeartbeatAt) {
		t.Fatalf("claim %v -> %v heartbeat %v -> %v", before.ClaimExpires, after.ClaimExpires, before.LastHeartbeatAt, after.LastHeartbeatAt)
	}
	if !reflect.DeepEqual(boardEvents(t, tid), eventsBefore) {
		t.Fatal("events changed")
	}
}

func TestDelegateChildAttachURLGuardLeavesNoRowOrFile(t *testing.T) {
	tid, _, attachmentsRoot := runningKanbanTask(t)
	prev := downloadURLWithCapFn
	t.Cleanup(func() { downloadURLWithCapFn = prev })
	downloadURLWithCapFn = func(context.Context, *tools.Env, string, int64) ([]byte, *string, error) {
		t.Error("delegated child guard must run before URL download")
		return nil, nil, errors.New("forbidden download")
	}
	ctx := tools.WithDelegatedChildContext(context.Background(), true)
	payload := decodeToolResult(t, kanbanToolRaw(t, ctx, nil, "kanban_attach_url", `{"task_id": "`+tid+`", "url": "https://example.com/leak.txt"}`))
	if e := errText(payload); !strings.Contains(e, "delegate_task child") {
		t.Fatal(payload)
	}
	if atts := boardAttachments(t, tid); len(atts) != 0 {
		t.Fatal(atts)
	}
	if entries, err := os.ReadDir(filepath.Join(attachmentsRoot, tid)); err == nil && len(entries) != 0 {
		t.Fatal(entries)
	}
}

func TestDelegatedChildContextSuppressesEnvGatedKanbanTools(t *testing.T) {
	clearKanbanEnv(t)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_parent")
	t.Setenv("ICEBATH_KANBAN_RUN_ID", "123")
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	tools.InvalidateCheckCache()
	tools.ClearToolDefinitionsCache()
	ctx := tools.WithDelegatedChildContext(context.Background(), true)
	schema := tools.Default.ToolDefinitions(ctx, nil, tools.DefinitionOptions{Enabled: []string{"terminal"}, Quiet: true})
	names := map[string]bool{}
	var kanban []string
	for _, s := range schema {
		fn, isFn := s["function"].(map[string]any)
		if !isFn {
			continue
		}
		name, _ := fn["name"].(string)
		names[name] = true
		if strings.HasPrefix(name, "kanban_") {
			kanban = append(kanban, name)
		}
	}
	if !names["terminal"] || len(kanban) != 0 {
		t.Fatal(names, kanban)
	}
}

// Goal mode task shaped like the SimpleNamespace the hermes gate tests pass
func goalTask(tid string) *Task {
	return &Task{ID: tid, Title: "goal title", Body: strp("goal body"), GoalMode: true}
}

func TestToolGateTransportFailureFailsOpenButGenuineVerdictRejects(t *testing.T) {
	k := gateCall(nil)
	patchGoalJudge(t, true, func(context.Context, string, string, goals.JudgeOptions) goals.JudgeResult {
		return goals.JudgeResult{Verdict: "continue", Reason: "judge error: BadRequestError", TransportFailed: true}
	})
	if err := goalGate(k, "kanban_complete", goalTask("task-1"), "task-1", "ev"); err != nil {
		t.Fatal(err)
	}
	judgeGoalFn = func(context.Context, string, string, goals.JudgeOptions) goals.JudgeResult {
		return goals.JudgeResult{Verdict: "continue", Reason: "goal not met yet"}
	}
	var rejected *kanbanReject
	if err := goalGate(k, "kanban_complete", goalTask("task-1"), "task-1", "ev"); !errors.As(err, &rejected) {
		t.Fatal(err)
	}
}

func TestToolGateBindsPerTaskAffinityScope(t *testing.T) {
	var seen []string
	var goalsSeen []string
	patchGoalJudge(t, true, func(ctx context.Context, goal, lastResponse string, _ goals.JudgeOptions) goals.JudgeResult {
		seen = append(seen, provider.PortalAffinityScope(ctx))
		goalsSeen = append(goalsSeen, goal+"|"+lastResponse)
		return goals.JudgeResult{Verdict: "done", Reason: "ok"}
	})
	if err := goalGate(gateCall(nil), "kanban_complete", goalTask("task-9"), "task-9", "ev"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "kanban:task-9" || goalsSeen[0] != "goal title\n\ngoal body|ev" {
		t.Fatal(seen, goalsSeen)
	}
	if scope := provider.PortalAffinityScope(context.Background()); scope != "" {
		t.Fatal(scope)
	}
	bound := provider.WithPortalAffinityScope(context.Background(), "outer-conversation")
	if err := goalGate(&kanbanCall{ctx: bound}, "kanban_complete", goalTask("task-9"), "task-9", "ev"); err != nil {
		t.Fatal(err)
	}
	if seen[len(seen)-1] != "outer-conversation" {
		t.Fatal(seen)
	}
}

func TestGoalGateMessagesFillReasonAndTaskPerVerdict(t *testing.T) {
	k := gateCall(nil)
	verdict := "blocked"
	patchGoalJudge(t, true, func(context.Context, string, string, goals.JudgeOptions) goals.JudgeResult {
		return goals.JudgeResult{Verdict: verdict, Reason: "no {tid} here"}
	})
	payload := func(tool string) string {
		var rejected *kanbanReject
		if err := goalGate(k, tool, goalTask("t_g"), "t_g", "ev"); !errors.As(err, &rejected) {
			t.Fatal(tool, err)
		}
		return rejected.payload
	}
	want := func(message string) string {
		return tools.ToolResult(pyjson.ObjectOf("error", message))
	}
	if got := payload("kanban_request_review"); got != want("Goal review handoff rejected: judge ruled the goal unachievable — no {tid} here. Record the block with kanban_block instead of requesting review.") {
		t.Fatal(got)
	}
	verdict = "wait"
	if got := payload("kanban_complete"); got != want("Goal completion rejected by judge: no {tid} here. To proceed, either: (1) provide explicit acceptance evidence in your summary matching the task's criteria, or (2) create continuation tasks with parents=[t_g] and keep this task alive.") {
		t.Fatal(got)
	}
	patchGoalJudge(t, false, nil)
	if err := goalGate(k, "kanban_complete", goalTask("t_g"), "t_g", "ev"); err != nil {
		t.Fatal("an unreachable judge must fail open", err)
	}
}
