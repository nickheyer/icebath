package kanban

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Raw JSON result of a registered kanban tool run on arguments the way a model call carries them
func kanbanToolRaw(t *testing.T, ctx context.Context, env *tools.Env, name, rawArgs string) string {
	t.Helper()
	def := tools.Default.Get(name)
	if def == nil {
		t.Fatalf("%s is not registered", name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		t.Fatal(err)
	}
	return def.Handler(ctx, &tools.Call{Name: name, Args: args, RawArgs: rawArgs, Env: env})
}

// Decoded result of a kanban tool run outside any delegate or session binding
func runKanbanTool(t *testing.T, name, rawArgs string) map[string]any {
	t.Helper()
	return decodeToolResult(t, kanbanToolRaw(t, context.Background(), nil, name, rawArgs))
}

// Tool result text decoded as a JSON dict
func decodeToolResult(t *testing.T, out string) map[string]any {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	return d
}

// Error text of a tool result, empty when it carries none
func errText(d map[string]any) string {
	s, _ := d["error"].(string)
	return s
}

// JSON text of a value for comparison with a Python literal
func jsonText(t *testing.T, v any) string {
	t.Helper()
	s, err := pyjson.Dumps(v)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Unsets an environment variable for the test, restoring it after
func unsetEnv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	os.Unsetenv(name)
}

// Worker fixture of the kanban tool tests, a claimed task with its run id pinned and no session id
func toolWorkerEnv(t *testing.T) string {
	t.Helper()
	tid := workerEnv(t)
	unsetEnv(t, "ICEBATH_SESSION_ID")
	unsetEnv(t, "ICEBATH_KANBAN_HOME")
	return tid
}

// Board task created on the default board, failing the test on error
func mustCreateTask(t *testing.T, o CreateTaskOptions) string {
	t.Helper()
	conn := mustConnect(t, "", nil)
	defer conn.Close()
	tid, err := CreateTask(context.Background(), conn, o)
	if err != nil {
		t.Fatal(err)
	}
	return tid
}

// Task row on the default board
func boardTask(t *testing.T, tid string) *Task {
	t.Helper()
	conn := mustConnect(t, "", nil)
	defer conn.Close()
	return mustTask(t, conn, tid)
}

// Events of a task on the default board
func boardEvents(t *testing.T, tid string) []*Event {
	t.Helper()
	conn := mustConnect(t, "", nil)
	defer conn.Close()
	events, err := ListEvents(context.Background(), conn, tid)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// Latest run of a task on the default board
func boardLatestRun(t *testing.T, tid string) *Run {
	t.Helper()
	conn := mustConnect(t, "", nil)
	defer conn.Close()
	run, err := LatestRun(context.Background(), conn, tid)
	if err != nil || run == nil {
		t.Fatalf("latest run %v %v", run, err)
	}
	return run
}

// Comments of a task on the default board
func boardComments(t *testing.T, tid string) []*Comment {
	t.Helper()
	conn := mustConnect(t, "", nil)
	defer conn.Close()
	comments, err := ListComments(context.Background(), conn, tid)
	if err != nil {
		t.Fatal(err)
	}
	return comments
}

// Attachments of a task on the default board
func boardAttachments(t *testing.T, tid string) []*Attachment {
	t.Helper()
	conn := mustConnect(t, "", nil)
	defer conn.Close()
	atts, err := ListAttachments(context.Background(), conn, tid)
	if err != nil {
		t.Fatal(err)
	}
	return atts
}

// Pointer to a string literal
func strp(s string) *string { return &s }

func TestKanbanToolsHiddenWithoutEnvVar(t *testing.T) {
	clearKanbanEnv(t)
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	tools.InvalidateCheckCache()
	ctx := context.Background()
	names := map[string]bool{}
	for _, n := range tools.Default.ResolveToolset(ctx, "", "icebath-cli") {
		names[n] = true
	}
	var kanban []string
	for _, s := range tools.Default.Definitions(ctx, nil, names, true) {
		fn, isFn := s["function"].(map[string]any)
		if !isFn {
			continue
		}
		if name, _ := fn["name"].(string); strings.HasPrefix(name, "kanban_") {
			kanban = append(kanban, name)
		}
	}
	if len(kanban) != 0 {
		t.Fatalf("kanban tools leaked into normal chat schema: %v", kanban)
	}
}

func TestShowDefaultsToEnvTaskID(t *testing.T) {
	tid := toolWorkerEnv(t)
	d := runKanbanTool(t, "kanban_show", `{}`)
	task, _ := d["task"].(map[string]any)
	if task == nil || task["id"] != tid || task["status"] != "running" {
		t.Fatalf("task %v", d)
	}
	if _, has := d["worker_context"]; !has {
		t.Fatal("worker_context missing")
	}
	if _, has := d["runs"]; !has {
		t.Fatal("runs missing")
	}
}

func TestShowBareCallOutsideWorkerReturnsOrientationNotError(t *testing.T) {
	toolWorkerEnv(t)
	unsetEnv(t, "ICEBATH_KANBAN_TASK")
	out := runKanbanTool(t, "kanban_show", `{}`)
	if _, has := out["error"]; has {
		t.Fatal(out)
	}
	if out["current_task"] != nil {
		t.Fatal(out)
	}
	if hint, _ := out["hint"].(string); !strings.Contains(hint, "kanban_list") {
		t.Fatal(out)
	}
}

func TestCommentBareCallOutsideWorkerNamesKanbanList(t *testing.T) {
	toolWorkerEnv(t)
	unsetEnv(t, "ICEBATH_KANBAN_TASK")
	out := runKanbanTool(t, "kanban_comment", `{"body": "hello"}`)
	e := errText(out)
	if e == "" || !strings.Contains(e, "kanban_list") || strings.Contains(e, "set ICEBATH_KANBAN_TASK") {
		t.Fatal(out)
	}
}

func TestWorkerToolBareCallOutsideWorkerErrorIsActionable(t *testing.T) {
	toolWorkerEnv(t)
	unsetEnv(t, "ICEBATH_KANBAN_TASK")
	for _, name := range []string{"kanban_heartbeat", "kanban_attachments"} {
		out := runKanbanTool(t, name, `{}`)
		e := errText(out)
		if e == "" || !strings.Contains(e, "kanban_list") || strings.Contains(e, "set ICEBATH_KANBAN_TASK") {
			t.Fatal(name, out)
		}
	}
}

func TestKanbanTaskIDDescriptionsStateTheNonWorkerCase(t *testing.T) {
	var seen []string
	for _, tool := range kanbanTools {
		params := tool.schema["parameters"].(map[string]any)
		props := params["properties"].(map[string]any)
		prop, _ := props["task_id"].(map[string]any)
		desc, _ := prop["description"].(string)
		if !strings.Contains(desc, "ICEBATH_KANBAN_TASK") {
			continue
		}
		if !strings.Contains(desc, "worker") {
			t.Fatal(tool.schema["name"], "env default promised without the non-worker case (#91431)")
		}
		if !strings.Contains(desc, "no default") && !strings.Contains(desc, "orientation") {
			t.Fatal(tool.schema["name"], "must state what a non-worker bare call does (#91431)")
		}
		seen = append(seen, tool.schema["name"].(string))
	}
	if !slices.Contains(seen, "kanban_show") {
		t.Fatal(seen)
	}
}

func TestListFiltersTasks(t *testing.T) {
	toolWorkerEnv(t)
	unsetEnv(t, "ICEBATH_KANBAN_TASK")
	a := mustCreateTask(t, CreateTaskOptions{Title: "alpha", Assignee: strp("factory"), Priority: 5})
	b := mustCreateTask(t, CreateTaskOptions{Title: "beta", Assignee: strp("reviewer")})
	c := mustCreateTask(t, CreateTaskOptions{Title: "gamma", Assignee: strp("factory"), Tenant: strp("other")})
	d := runKanbanTool(t, "kanban_list", `{"assignee": "factory", "status": "ready", "limit": 10}`)
	tasks, _ := d["tasks"].([]any)
	var ids []string
	for _, task := range tasks {
		ids = append(ids, task.(map[string]any)["id"].(string))
	}
	if !slices.Equal(ids, []string{a, c}) || d["count"] != 2.0 {
		t.Fatalf("list %v", d)
	}
	first := tasks[0].(map[string]any)
	if first["title"] != "alpha" || first["parent_count"] != 0.0 || slices.Contains(ids, b) {
		t.Fatalf("first %v", first)
	}
	tenantOut := runKanbanTool(t, "kanban_list", `{"assignee": "factory", "status": "ready", "tenant": "other"}`)
	var tenantIDs []string
	for _, task := range tenantOut["tasks"].([]any) {
		tenantIDs = append(tenantIDs, task.(map[string]any)["id"].(string))
	}
	if !slices.Equal(tenantIDs, []string{c}) {
		t.Fatalf("tenant list %v", tenantOut)
	}
}

func TestCompleteHappyPath(t *testing.T) {
	tid := toolWorkerEnv(t)
	d := runKanbanTool(t, "kanban_complete", `{"summary": "got the thing done", "metadata": {"files": 2}}`)
	if d["ok"] != true || d["task_id"] != tid {
		t.Fatal(d)
	}
	run := boardLatestRun(t, tid)
	if run.Outcome == nil || *run.Outcome != "completed" || run.Summary == nil || *run.Summary != "got the thing done" {
		t.Fatalf("run %v %v", run.Outcome, run.Summary)
	}
	if got := jsonText(t, run.Metadata); got != `{"files": 2}` {
		t.Fatal(got)
	}
}

func TestCompleteRetryWithEmptyCreatedCardsSucceeds(t *testing.T) {
	tid := toolWorkerEnv(t)
	rejected := runKanbanTool(t, "kanban_complete", `{"summary": "oops", "created_cards": ["t_phantomdeadbeef"]}`)
	if errText(rejected) == "" {
		t.Fatal(rejected)
	}
	ok := runKanbanTool(t, "kanban_complete", `{"summary": "retry without claims", "created_cards": []}`)
	if ok["ok"] != true {
		t.Fatal(ok)
	}
	if status := boardTask(t, tid).Status; status != "done" {
		t.Fatal(status)
	}
}

func TestCompleteReportsRegisteredAttachments(t *testing.T) {
	tid := toolWorkerEnv(t)
	ctx := context.Background()
	conn := mustConnect(t, "", nil)
	ws, err := ResolveWorkspace(ctx, mustTask(t, conn, tid), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetWorkspacePath(ctx, conn, tid, ws); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	artifact := filepath.Join(ws, "corpus.json")
	if err := os.WriteFile(artifact, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"summary": "done", "artifacts": []string{artifact}})
	d := runKanbanTool(t, "kanban_complete", string(raw))
	if d["ok"] != true {
		t.Fatal(d)
	}
	atts, _ := d["attachments"].([]any)
	if len(atts) != 1 {
		t.Fatal(d)
	}
	a := atts[0].(map[string]any)
	if a["filename"] != "corpus.json" || a["size"] != 2.0 || a["uploaded_by"] != "kanban_complete" {
		t.Fatal(a)
	}
	readback := runKanbanTool(t, "kanban_attachments", `{"task_id": "`+tid+`"}`)
	if !reflect.DeepEqual(readback["attachments"], d["attachments"]) {
		t.Fatalf("readback %v, completion %v", readback["attachments"], d["attachments"])
	}
}

// Installed profile marker of the review tests under the patched user home
func installVerifierProfile(t *testing.T) {
	t.Helper()
	dir := filepath.Join(os.Getenv("ICEBATH_HOME"), "profiles", "verifier")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRequestReviewRejectsUnknownReviewerWithoutMutation(t *testing.T) {
	tid := toolWorkerEnv(t)
	installVerifierProfile(t)
	before := boardTask(t, tid)
	beforeEvents := boardEvents(t, tid)
	t.Setenv("ICEBATH_KANBAN_RUN_ID", strconv.FormatInt(*before.CurrentRunID, 10))
	out := runKanbanTool(t, "kanban_request_review", `{"summary": "Ready for review.", "reviewer": "reviewer"}`)
	if e := errText(out); !strings.Contains(e, "'reviewer'") || !strings.Contains(e, "verifier") {
		t.Fatal(out)
	}
	after := boardTask(t, tid)
	if after.Status != "running" || after.Assignee == nil || *after.Assignee != "test-worker" || *after.CurrentRunID != *before.CurrentRunID {
		t.Fatalf("after %q %v %v", after.Status, after.Assignee, after.CurrentRunID)
	}
	if !reflect.DeepEqual(boardEvents(t, tid), beforeEvents) {
		t.Fatal("events changed")
	}
}

func TestRequestReviewAcceptsInstalledProfile(t *testing.T) {
	tid := toolWorkerEnv(t)
	installVerifierProfile(t)
	t.Setenv("ICEBATH_KANBAN_RUN_ID", strconv.FormatInt(*boardTask(t, tid).CurrentRunID, 10))
	out := runKanbanTool(t, "kanban_request_review", `{"summary": "Ready for review.", "reviewer": "verifier"}`)
	if out["ok"] != true {
		t.Fatal(out)
	}
	task := boardTask(t, tid)
	if task.Status != "review" || task.Assignee == nil || *task.Assignee != "verifier" {
		t.Fatalf("task %q %v", task.Status, task.Assignee)
	}
}

func TestUnboundWorkerCannotMutateCard(t *testing.T) {
	tid := toolWorkerEnv(t)
	unsetEnv(t, "ICEBATH_KANBAN_RUN_ID")
	for _, c := range []struct{ name, args string }{
		{"kanban_complete", `{"summary": "stale worker says done"}`},
		{"kanban_block", `{"reason": "stale worker blocks"}`},
		{"kanban_schedule", `{"reason": "stale worker parks"}`},
		{"kanban_request_review", `{"summary": "stale worker hands off"}`},
		{"kanban_request_changes", `{"reason": "stale worker requests changes"}`},
	} {
		if out := runKanbanTool(t, c.name, c.args); !strings.Contains(errText(out), "refused") {
			t.Fatalf("%s did not refuse: %v", c.name, out)
		}
	}
	task := boardTask(t, tid)
	if task.Status != "running" || task.CurrentRunID == nil {
		t.Fatalf("task %q %v", task.Status, task.CurrentRunID)
	}
	t.Setenv("ICEBATH_KANBAN_RUN_ID", strconv.FormatInt(*task.CurrentRunID, 10))
	if out := runKanbanTool(t, "kanban_complete", `{"summary": "bound worker done"}`); out["ok"] != true {
		t.Fatal(out)
	}
	if status := boardTask(t, tid).Status; status != "done" {
		t.Fatal(status)
	}
}

func TestMalformedRunIDRefusedButNonlifecycleAllowed(t *testing.T) {
	tid := toolWorkerEnv(t)
	t.Setenv("ICEBATH_KANBAN_RUN_ID", "not-an-int")
	if out := runKanbanTool(t, "kanban_complete", `{"summary": "stale worker says done"}`); !strings.Contains(errText(out), "refused") {
		t.Fatal(out)
	}
	if out := runKanbanTool(t, "kanban_schedule", `{"reason": "stale worker parks"}`); !strings.Contains(errText(out), "refused") {
		t.Fatal(out)
	}
	if out := runKanbanTool(t, "kanban_heartbeat", `{}`); out["ok"] != true {
		t.Fatal(out)
	}
	if status := boardTask(t, tid).Status; status != "running" {
		t.Fatal(status)
	}
}

// Isolated home with one claimed goal mode task pinned as this worker's task and run
func goalModeWorkerEnv(t *testing.T, title, body string) string {
	t.Helper()
	clearKanbanEnv(t)
	setupKanbanHome(t)
	t.Setenv("ICEBATH_PROFILE", "test-worker")
	unsetEnv(t, "ICEBATH_SESSION_ID")
	ctx := context.Background()
	if _, err := InitDB(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
	conn := mustConnect(t, "", nil)
	defer conn.Close()
	tid, err := CreateTask(ctx, conn, CreateTaskOptions{Title: title, Assignee: strp("test-worker"), Body: &body, GoalMode: true})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := ClaimTask(ctx, conn, tid, nil, nil); err != nil || claimed == nil {
		t.Fatalf("claim %v %v", claimed, err)
	}
	runID, err := currentRunID(ctx, conn, tid)
	if err != nil || runID == nil {
		t.Fatalf("run id %v %v", runID, err)
	}
	t.Setenv("ICEBATH_KANBAN_TASK", tid)
	t.Setenv("ICEBATH_KANBAN_RUN_ID", strconv.FormatInt(*runID, 10))
	return tid
}

// Installs a judge and its availability probe for one test
func patchGoalJudge(t *testing.T, available bool, judge func(ctx context.Context, goal, lastResponse string, o goals.JudgeOptions) goals.JudgeResult) {
	t.Helper()
	prevJudge, prevAvailable := judgeGoalFn, goalJudgeAvailableFn
	t.Cleanup(func() { judgeGoalFn, goalJudgeAvailableFn = prevJudge, prevAvailable })
	if judge != nil {
		judgeGoalFn = judge
	}
	goalJudgeAvailableFn = func(context.Context) bool { return available }
}

func TestCompleteGoalModeRejectedByJudge(t *testing.T) {
	tid := goalModeWorkerEnv(t, "goal-mode-test", "Must achieve X with verified evidence.")
	patchGoalJudge(t, true, func(context.Context, string, string, goals.JudgeOptions) goals.JudgeResult {
		return goals.JudgeResult{Verdict: "continue", Reason: "missing verification evidence"}
	})
	d := runKanbanTool(t, "kanban_complete", `{"summary": "I did some stuff but not X"}`)
	e := errText(d)
	if !strings.Contains(e, "Goal completion rejected by judge") || !strings.Contains(e, "missing verification evidence") ||
		!strings.Contains(e, "parents=["+tid+"]") {
		t.Fatal(d)
	}
	if status := boardTask(t, tid).Status; status != "running" {
		t.Fatal(status)
	}
}

func TestBlockHappyPath(t *testing.T) {
	tid := toolWorkerEnv(t)
	if d := runKanbanTool(t, "kanban_block", `{"reason": "need clarification"}`); d["ok"] != true {
		t.Fatal(d)
	}
	if status := boardTask(t, tid).Status; status != "blocked" {
		t.Fatal(status)
	}
}

func TestScheduleParksCurrentWorkerWithReason(t *testing.T) {
	tid := toolWorkerEnv(t)
	reason := "SCHEDULED_UNTIL=2026-09-13T00:00:00Z waiting for reconnect"
	entry := tools.Default.Get("kanban_schedule")
	if entry == nil || entry.Toolset != "kanban" {
		t.Fatal(entry)
	}
	raw := `{"reason": "` + reason + `"}`
	var args map[string]any
	_ = json.Unmarshal([]byte(raw), &args)
	out := decodeToolResult(t, entry.Handler(context.Background(), &tools.Call{Name: "kanban_schedule", Args: args, RawArgs: raw}))
	want := map[string]any{"ok": true, "task_id": tid, "run_id": out["run_id"], "status": "scheduled", "reason": reason}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("got %v want %v", out, want)
	}
	if status := boardTask(t, tid).Status; status != "scheduled" {
		t.Fatal(status)
	}
	run := boardLatestRun(t, tid)
	if run.Outcome == nil || *run.Outcome != "scheduled" || run.Summary == nil || *run.Summary != reason {
		t.Fatalf("run %v %v", run.Outcome, run.Summary)
	}
	found := false
	for _, e := range boardEvents(t, tid) {
		if e.Kind == "scheduled" && jsonText(t, e.Payload) == jsonText(t, pyjson.ObjectOf("reason", reason)) {
			found = true
		}
	}
	if !found {
		t.Fatal("scheduled event missing")
	}
}

func TestScheduleRejectsInvalidReasonAndUnownedContexts(t *testing.T) {
	tid := toolWorkerEnv(t)
	other := mustCreateTask(t, CreateTaskOptions{Title: "sibling", Assignee: strp("peer")})
	invalid := runKanbanTool(t, "kanban_schedule", `{"reason": {"until": "tomorrow"}}`)
	foreign := runKanbanTool(t, "kanban_schedule", `{"task_id": "`+other+`", "reason": "wait"}`)
	prev := delegationCtx
	t.Cleanup(func() { delegationCtx = prev })
	delegationCtx = func(_ context.Context, _ *tools.Env, predicate string, _ bool) bool {
		return predicate == "is_delegated_child_process_context"
	}
	delegated := runKanbanTool(t, "kanban_schedule", `{"task_id": "`+tid+`", "reason": "wait"}`)
	if !strings.Contains(errText(invalid), "reason must be a string") {
		t.Fatal(invalid)
	}
	if !strings.Contains(errText(foreign), "refusing to mutate") {
		t.Fatal(foreign)
	}
	if !strings.Contains(errText(delegated), "delegate_task child agents are not Kanban run owners") {
		t.Fatal(delegated)
	}
	if status := boardTask(t, tid).Status; status != "running" {
		t.Fatal(status)
	}
	if status := boardTask(t, other).Status; status != "ready" {
		t.Fatal(status)
	}
}

func TestBlockGoalModeRejectsMissingKind(t *testing.T) {
	tid := goalModeWorkerEnv(t, "goal-mode-block-test", "Must achieve X.")
	d := runKanbanTool(t, "kanban_block", `{"reason": "giving up"}`)
	if !strings.Contains(errText(d), "goal_mode") {
		t.Fatal(d)
	}
	if status := boardTask(t, tid).Status; status != "running" {
		t.Fatal(status)
	}
}

func TestBlockGoalModeRejectsDisallowedKind(t *testing.T) {
	tid := goalModeWorkerEnv(t, "goal-mode-block-test", "Must achieve X.")
	for _, kind := range []string{"capability", "transient"} {
		if d := runKanbanTool(t, "kanban_block", `{"reason": "blocked", "kind": "`+kind+`"}`); errText(d) == "" {
			t.Fatalf("kind=%s should be rejected for goal_mode", kind)
		}
	}
	if status := boardTask(t, tid).Status; status != "running" {
		t.Fatal(status)
	}
}

func TestScheduleGoalModeRefused(t *testing.T) {
	tid := goalModeWorkerEnv(t, "goal-mode-block-test", "Must achieve X.")
	if d := runKanbanTool(t, "kanban_schedule", `{"reason": "waiting for CI"}`); !strings.Contains(errText(d), "goal_mode") {
		t.Fatal(d)
	}
	if status := boardTask(t, tid).Status; status != "running" {
		t.Fatal(status)
	}
}

func TestScheduleExposedToCodexRuntimeWorkers(t *testing.T) {
	if !slices.Contains(provider.CodexToolsExposedTools(), "kanban_schedule") {
		t.Fatal(provider.CodexToolsExposedTools())
	}
}

func TestBlockDependencyWithoutOpenParentIsRekinded(t *testing.T) {
	toolWorkerEnv(t)
	d := runKanbanTool(t, "kanban_block", `{"reason": "upstream input is missing", "kind": "dependency"}`)
	if d["ok"] != true || d["status"] != "blocked" || d["block_kind"] != "needs_input" || d["requested_kind"] != "dependency" {
		t.Fatal(d)
	}
	if note, _ := d["note"].(string); !strings.Contains(note, "no parent is open") {
		t.Fatal(d)
	}
}

func TestHeartbeatExtendsClaimExpires(t *testing.T) {
	tid := toolWorkerEnv(t)
	conn := mustConnect(t, "", nil)
	mustExec(t, conn, "UPDATE tasks SET claim_expires = 1 WHERE id = '"+tid+"'")
	before := mustTask(t, conn, tid).ClaimExpires
	conn.Close()
	if before == nil || *before != 1 {
		t.Fatal(before)
	}
	if out := runKanbanTool(t, "kanban_heartbeat", `{"note": "still alive"}`); out["ok"] != true {
		t.Fatal(out)
	}
	after := boardTask(t, tid).ClaimExpires
	now := time.Now().Unix()
	if after == nil || *after <= *before {
		t.Fatalf("claim_expires did not advance (%d -> %v); workers would be reclaimed at TTL despite heartbeating", *before, after)
	}
	if *after < now+DefaultClaimTTLSeconds/2 {
		t.Fatalf("claim_expires=%d is suspiciously close to now=%d; expected at least now + %d", *after, now, DefaultClaimTTLSeconds/2)
	}
}

func TestCommentRejectsCallerSuppliedAuthor(t *testing.T) {
	tid := toolWorkerEnv(t)
	out := runKanbanTool(t, "kanban_comment", `{"task_id": "`+tid+`", "body": "hi", "author": "icebath-system"}`)
	if !strings.Contains(errText(out), "author") {
		t.Fatal(out)
	}
	if comments := boardComments(t, tid); len(comments) != 0 {
		t.Fatal(comments)
	}
	if out := runKanbanTool(t, "kanban_comment", `{"task_id": "`+tid+`", "body": "hi"}`); out["ok"] != true {
		t.Fatal(out)
	}
	if comments := boardComments(t, tid); len(comments) == 0 || comments[0].Author != "test-worker" {
		t.Fatal(comments)
	}
}

func TestCreateHappyPath(t *testing.T) {
	tid := toolWorkerEnv(t)
	d := runKanbanTool(t, "kanban_create", `{"title": "child task", "assignee": "peer", "parents": ["`+tid+`"]}`)
	if d["ok"] != true || d["task_id"] == "" || d["task_id"] == nil {
		t.Fatal(d)
	}
	if d["status"] != "todo" || d["gated"] != true || d["gated_by"] != tid {
		t.Fatal(d)
	}
	child := boardTask(t, d["task_id"].(string))
	if child.Title != "child task" || child.Assignee == nil || *child.Assignee != "peer" {
		t.Fatalf("child %q %v", child.Title, child.Assignee)
	}
}

func TestCreateExplicitScratchIgnoresAmbientBoardProject(t *testing.T) {
	for _, explicit := range []string{`"workspace_kind": "scratch"`, `"project": ""`} {
		for _, targetScoped := range []bool{false, true} {
			t.Run(explicit+"/"+strconv.FormatBool(targetScoped), func(t *testing.T) {
				toolWorkerEnv(t)
				ctx := context.Background()
				repo := filepath.Join(filepath.Dir(os.Getenv("ICEBATH_HOME")), "repo")
				if err := os.MkdirAll(repo, 0o755); err != nil {
					t.Fatal(err)
				}
				pdb, err := tools.OpenProjectsDB(ctx, &tools.Env{Home: os.Getenv("ICEBATH_HOME")})
				if err != nil {
					t.Fatal(err)
				}
				projectID, err := pdb.CreateProject("Ambient", tools.CreateProjectOptions{PrimaryPath: repo})
				pdb.Close()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := WriteBoardMetadata(ctx, BoardArg("default"), BoardMetadataUpdate{ProjectID: &projectID}); err != nil {
					t.Fatal(err)
				}
				targetProject := ""
				if targetScoped {
					targetProject = projectID
				}
				if _, err := CreateBoard(ctx, "target", BoardCreateOptions{Name: strp("Target"), ProjectID: &targetProject}); err != nil {
					t.Fatal(err)
				}
				create := func(extra string) (any, any) {
					args := `{"board": "target", "title": "card", "assignee": "peer"`
					if extra != "" {
						args += ", " + extra
					}
					result := runKanbanTool(t, "kanban_create", args+"}")
					if result["ok"] != true {
						t.Fatal(result)
					}
					return result["workspace_kind"], result["project_id"]
				}
				if kind, project := create(explicit); kind != "scratch" || project != nil {
					t.Fatalf("explicit %v %v", kind, project)
				}
				kind, project := create("")
				if targetScoped && (kind != "worktree" || project != projectID) {
					t.Fatalf("scoped %v %v", kind, project)
				}
				if !targetScoped && (kind != "scratch" || project != nil) {
					t.Fatalf("unscoped %v %v", kind, project)
				}
			})
		}
	}
}

func TestLinkRunningChildAllowsOwnerButRejectsForeign(t *testing.T) {
	tid := toolWorkerEnv(t)
	ctx := context.Background()
	conn := mustConnect(t, "", nil)
	ownParent, err := CreateTask(ctx, conn, CreateTaskOptions{Title: "own review"})
	if err != nil {
		t.Fatal(err)
	}
	ownRunID := mustTask(t, conn, tid).CurrentRunID
	foreignParent, err := CreateTask(ctx, conn, CreateTaskOptions{Title: "foreign review"})
	if err != nil {
		t.Fatal(err)
	}
	foreignChild, err := CreateTask(ctx, conn, CreateTaskOptions{Title: "foreign worker"})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := ClaimTask(ctx, conn, foreignChild, nil, strp("other")); err != nil || claimed == nil {
		t.Fatalf("claim %v %v", claimed, err)
	}
	conn.Close()
	t.Setenv("ICEBATH_KANBAN_RUN_ID", strconv.FormatInt(*ownRunID, 10))
	own := runKanbanTool(t, "kanban_link", `{"parent_id": "`+ownParent+`", "child_id": "`+tid+`"}`)
	foreign := runKanbanTool(t, "kanban_link", `{"parent_id": "`+foreignParent+`", "child_id": "`+foreignChild+`"}`)
	if own["ok"] != true {
		t.Fatal(own)
	}
	if !strings.Contains(errText(foreign), "child is already running") {
		t.Fatal(foreign)
	}
	conn = mustConnect(t, "", nil)
	defer conn.Close()
	if parents, _ := ParentIDs(ctx, conn, tid); !slices.Equal(parents, []string{ownParent}) {
		t.Fatal(parents)
	}
	if parents, _ := ParentIDs(ctx, conn, foreignChild); len(parents) != 0 {
		t.Fatal(parents)
	}
}

func TestUnblockHappyPath(t *testing.T) {
	toolWorkerEnv(t)
	unsetEnv(t, "ICEBATH_KANBAN_TASK")
	tid := mustCreateTask(t, CreateTaskOptions{Title: "blocked", Assignee: strp("worker")})
	conn := mustConnect(t, "", nil)
	if err := BlockTask(context.Background(), conn, tid, "waiting", nil); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	d := runKanbanTool(t, "kanban_unblock", `{"task_id": "`+tid+`"}`)
	if d["ok"] != true || d["status"] != "ready" {
		t.Fatal(d)
	}
	if status := boardTask(t, tid).Status; status != "ready" {
		t.Fatal(status)
	}
}

func TestUnblockWithPendingParentsReturnsTodo(t *testing.T) {
	clearKanbanEnv(t)
	setupKanbanHome(t)
	t.Setenv("ICEBATH_PROFILE", "orchestrator")
	ctx := context.Background()
	if _, err := InitDB(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
	parent := mustCreateTask(t, CreateTaskOptions{Title: "parent", Assignee: strp("worker")})
	child := mustCreateTask(t, CreateTaskOptions{Title: "child", Assignee: strp("worker"), Parents: []string{parent}})
	conn := mustConnect(t, "", nil)
	mustExec(t, conn, "UPDATE tasks SET status='blocked' WHERE id='"+child+"'")
	conn.Close()
	d := runKanbanTool(t, "kanban_unblock", `{"task_id": "`+child+`"}`)
	if d["ok"] != true || d["status"] != "todo" {
		t.Fatal(d)
	}
	if status := boardTask(t, child).Status; status != "todo" {
		t.Fatal(status)
	}
}

func TestWorkerLifecycleThroughTools(t *testing.T) {
	tid := toolWorkerEnv(t)
	show := runKanbanTool(t, "kanban_show", `{}`)
	if show["task"].(map[string]any)["id"] != tid {
		t.Fatal(show)
	}
	if out := runKanbanTool(t, "kanban_heartbeat", `{"note": "warming up"}`); out["ok"] != true {
		t.Fatal(out)
	}
	if out := runKanbanTool(t, "kanban_comment", `{"task_id": "`+tid+`", "body": "note: using stdlib sqlite3 bindings"}`); out["ok"] != true {
		t.Fatal(out)
	}
	childOut := runKanbanTool(t, "kanban_create", `{"title": "write integration test", "assignee": "qa", "parents": ["`+tid+`"]}`)
	if childOut["ok"] != true {
		t.Fatal(childOut)
	}
	childID := childOut["task_id"].(string)
	comp := runKanbanTool(t, "kanban_complete", `{"summary": "implemented + spawned QA follow-up", "metadata": {"child_task": "`+childID+`"}}`)
	if comp["ok"] != true {
		t.Fatal(comp)
	}
	parent := boardTask(t, tid)
	if parent.Status != "done" || parent.CurrentRunID != nil {
		t.Fatalf("parent %q %v", parent.Status, parent.CurrentRunID)
	}
	run := boardLatestRun(t, tid)
	if run.Outcome == nil || *run.Outcome != "completed" || jsonText(t, run.Metadata) != jsonText(t, pyjson.ObjectOf("child_task", childID)) {
		t.Fatalf("run %v %v", run.Outcome, run.Metadata)
	}
	if status := boardTask(t, childID).Status; status != "ready" {
		t.Fatalf("child should be ready after parent done, got %s", status)
	}
	if comments := boardComments(t, tid); len(comments) != 1 {
		t.Fatal(comments)
	}
	heartbeats := 0
	for _, e := range boardEvents(t, tid) {
		if e.Kind == "heartbeat" {
			heartbeats++
		}
	}
	if heartbeats != 1 {
		t.Fatal(heartbeats)
	}
}

func TestWorkerCompleteRejectsForeignTaskID(t *testing.T) {
	toolWorkerEnv(t)
	other := mustCreateTask(t, CreateTaskOptions{Title: "sibling"})
	conn := mustConnect(t, "", nil)
	mustExec(t, conn, "UPDATE tasks SET status='ready' WHERE id='"+other+"'")
	conn.Close()
	d := runKanbanTool(t, "kanban_complete", `{"task_id": "`+other+`", "summary": "HIJACK"}`)
	if d["ok"] == true || !strings.Contains(errText(d), "refusing to mutate") {
		t.Fatal(d)
	}
	if status := boardTask(t, other).Status; status != "ready" {
		t.Fatal(status)
	}
}

func TestWorkerCanCommentOnForeignTask(t *testing.T) {
	toolWorkerEnv(t)
	other := mustCreateTask(t, CreateTaskOptions{Title: "sibling"})
	d := runKanbanTool(t, "kanban_comment", `{"task_id": "`+other+`", "body": "handoff: see prior findings before starting"}`)
	if d["ok"] != true {
		t.Fatalf("cross-task comment must succeed: %v", d)
	}
	comments := boardComments(t, other)
	if len(comments) != 1 || comments[0].Author != "test-worker" || !strings.HasPrefix(comments[0].Body, "handoff:") {
		t.Fatal(comments)
	}
}

func TestWorkerUnblockRejectsForeignTaskID(t *testing.T) {
	toolWorkerEnv(t)
	other := mustCreateTask(t, CreateTaskOptions{Title: "blocked sibling", Assignee: strp("peer")})
	conn := mustConnect(t, "", nil)
	if err := BlockTask(context.Background(), conn, other, "waiting", nil); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	d := runKanbanTool(t, "kanban_unblock", `{"task_id": "`+other+`"}`)
	if e := errText(d); !strings.Contains(e, "orchestrator-only") && !strings.Contains(e, "refusing to mutate") {
		t.Fatalf("expected worker-rejection error, got %s", e)
	}
	if status := boardTask(t, other).Status; status != "blocked" {
		t.Fatal(status)
	}
}

func TestOrchestratorCompleteAnyTaskAllowed(t *testing.T) {
	clearKanbanEnv(t)
	setupKanbanHome(t)
	if _, err := InitDB(context.Background(), "", nil); err != nil {
		t.Fatal(err)
	}
	tid := mustCreateTask(t, CreateTaskOptions{Title: "child to close out"})
	conn := mustConnect(t, "", nil)
	mustExec(t, conn, "UPDATE tasks SET status='ready' WHERE id='"+tid+"'")
	conn.Close()
	d := runKanbanTool(t, "kanban_complete", `{"task_id": "`+tid+`", "summary": "orchestrator close"}`)
	if d["ok"] != true || d["task_id"] != tid {
		t.Fatal(d)
	}
}

func TestRunIDPastTheSQLiteRangeReachesTheBoardCallAndFailsAtItsBind(t *testing.T) {
	tid := toolWorkerEnv(t)
	t.Setenv("ICEBATH_KANBAN_RUN_ID", "9223372036854775808")
	const overflow = "Python int too large to convert to SQLite INTEGER"
	if got := errText(runKanbanTool(t, "kanban_heartbeat", `{"note": "alive"}`)); got != "kanban_heartbeat: "+overflow {
		t.Fatalf("heartbeat %q", got)
	}
	for _, c := range []struct{ name, args, want string }{
		{"kanban_complete", `{"summary": "done"}`, "kanban_complete: " + overflow},
		{"kanban_block", `{"reason": "stuck"}`, "kanban_block: " + overflow},
		{"kanban_schedule", `{"reason": "later"}`, "kanban_schedule: " + overflow},
		{"kanban_request_review", `{"summary": "ready"}`, "kanban_request_review: " + overflow},
		{"kanban_request_changes", `{"reason": "more"}`, "could not request changes for " + tid + ": run_id mismatch"},
	} {
		if got := errText(runKanbanTool(t, c.name, c.args)); got != c.want {
			t.Fatalf("%s answered %q want %q", c.name, got, c.want)
		}
	}
	if task := boardTask(t, tid); task.Status != "running" || task.CurrentRunID == nil {
		t.Fatalf("task moved to %q", task.Status)
	}
}
