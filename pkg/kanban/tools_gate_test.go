package kanban

import (
	"context"
	"encoding/json"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Tool call through the registry dispatch the way registry.dispatch runs a handler
func dispatchKanban(t *testing.T, name, rawArgs string) map[string]any {
	t.Helper()
	var args map[string]any
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		t.Fatal(err)
	}
	out := tools.NewDispatcher(tools.Default).RegistryDispatch(context.Background(), &tools.Call{Name: name, Args: args, RawArgs: rawArgs})
	return decodeToolResult(t, out)
}

// Kanban call of the gate helpers bound to an env with session values
func gateCall(env *tools.Env) *kanbanCall {
	return &kanbanCall{ctx: context.Background(), env: env, args: map[string]any{}}
}

func TestDefaultTaskIDNumericTaskIDIsCoerced(t *testing.T) {
	unsetEnv(t, "ICEBATH_KANBAN_TASK")
	k := gateCall(nil)
	if id, ok := defaultTaskID(k, int64(12345)); !ok || id != "12345" {
		t.Fatal(id, ok)
	}
	if id, ok := defaultTaskID(k, int64(0)); !ok || id != "0" {
		t.Fatal(id, ok)
	}
}

func TestDefaultTaskIDWhitespaceTaskIDIsStripped(t *testing.T) {
	unsetEnv(t, "ICEBATH_KANBAN_TASK")
	if id, ok := defaultTaskID(gateCall(nil), "  task-99  "); !ok || id != "task-99" {
		t.Fatal(id, ok)
	}
}

func TestDefaultTaskIDBlankTaskIDFallsBack(t *testing.T) {
	tid := toolWorkerEnv(t)
	prev := isDispatcherOwnedWorker
	t.Cleanup(func() { isDispatcherOwnedWorker = prev })
	isDispatcherOwnedWorker = func(context.Context, *tools.Env) bool { return true }
	k := gateCall(nil)
	for _, arg := range []any{"", "   ", nil} {
		if id, ok := defaultTaskID(k, arg); !ok || id != tid {
			t.Fatalf("%q -> %q %v", arg, id, ok)
		}
	}
}

// Environment of a dispatcher spawned kanban worker a cron job fires inside
func cronWorkerEnv(t *testing.T) *tools.Env {
	t.Helper()
	t.Setenv("ICEBATH_KANBAN_TASK", "t_worker_real_task")
	t.Setenv("ICEBATH_KANBAN_WORKSPACE", "/tmp/ws")
	t.Setenv("ICEBATH_KANBAN_RUN_ID", "42")
	t.Setenv("ICEBATH_KANBAN_CLAIM_LOCK", "lock-abc")
	t.Setenv("ICEBATH_KANBAN_BOARD", "team-alpha")
	return &tools.Env{Values: &sync.Map{}}
}

func TestKanbanGatesRespectContextTaskToolsHiddenFromCronAgent(t *testing.T) {
	env := cronWorkerEnv(t)
	if !checkKanbanMode(env) {
		t.Fatal("worker hidden its own lifecycle tools")
	}
	restore := tools.EnterNonDispatcherOwnedContext(env)
	defer restore()
	if checkKanbanMode(env) {
		t.Fatal("cron agent sees the worker's lifecycle tools")
	}
}

func TestKanbanGatesRespectContextCompleteDoesNotDefaultToWorkerTask(t *testing.T) {
	env := cronWorkerEnv(t)
	if id, ok := defaultTaskID(gateCall(env), nil); !ok || id != "t_worker_real_task" {
		t.Fatal(id, ok)
	}
	restore := tools.EnterNonDispatcherOwnedContext(env)
	defer restore()
	if id, ok := defaultTaskID(gateCall(env), nil); ok {
		t.Fatal("cron run inherited the worker task", id)
	}
}

func TestKanbanGatesRespectContextExplicitTaskIDStillHonoured(t *testing.T) {
	env := cronWorkerEnv(t)
	restore := tools.EnterNonDispatcherOwnedContext(env)
	defer restore()
	if id, ok := defaultTaskID(gateCall(env), "t_explicit"); !ok || id != "t_explicit" {
		t.Fatal(id, ok)
	}
}

func TestReviewUnknownArgumentRejectsBeforeHandoff(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_KANBAN_HOME", h)
	t.Cleanup(home.PatchUserHome(tmp))
	for _, name := range []string{"ICEBATH_KANBAN_TASK", "ICEBATH_KANBAN_DB", "ICEBATH_KANBAN_BOARD", "ICEBATH_KANBAN_RUN_ID"} {
		unsetEnv(t, name)
	}
	discardInitialized(t, mustDBPath(t, BoardArg("default")))
	ctx := context.Background()
	if _, err := InitDB(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
	conn := mustConnect(t, "", nil)
	tid, err := CreateTask(ctx, conn, CreateTaskOptions{Title: "review payload", Assignee: strp("builder"), WorkspaceKind: strp("scratch")})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := ClaimTask(ctx, conn, tid, nil, nil); err != nil || claimed == nil {
		t.Fatalf("claim %v %v", claimed, err)
	}
	root, err := WorkspacesRoot(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, tid)
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "UPDATE tasks SET workspace_path=? WHERE id=?", workspace, tid); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	artifact := filepath.Join(workspace, "result.txt")
	if err := os.WriteFile(artifact, []byte("verified result"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"task_id": tid, "summary": "verified", "summary_artifacts": []string{artifact}})
	result := dispatchKanban(t, "kanban_request_review", string(raw))
	status, attachments := boardTask(t, tid).Status, boardAttachments(t, tid)
	if !strings.Contains(errText(result), "summary_artifacts") {
		t.Fatal(result, status, attachments)
	}
	if status != "running" || len(attachments) != 0 {
		t.Fatal(status, attachments)
	}
	raw, _ = json.Marshal(map[string]any{"task_id": tid, "summary": "verified", "artifacts": []string{artifact},
		"metadata": map[string]any{"custom_key": "allowed"}})
	if corrected := dispatchKanban(t, "kanban_request_review", string(raw)); corrected["ok"] != true {
		t.Fatal(corrected)
	}
	if status := boardTask(t, tid).Status; status != "review" {
		t.Fatal(status)
	}
	if len(boardAttachments(t, tid)) == 0 {
		t.Fatal("no attachments staged")
	}
}

func TestRegisteredKanbanToolsRejectUnknownKeys(t *testing.T) {
	for _, tool := range kanbanTools {
		result := dispatchKanban(t, tool.name, `{"misspelled_argument": true}`)
		if e := errText(result); !strings.Contains(e, "unknown parameter") || !strings.Contains(e, "misspelled_argument") {
			t.Fatal(tool.name, result)
		}
	}
}

func TestUndeclaredInternalKeysSurviveTheStrictCheck(t *testing.T) {
	result := dispatchKanban(t, "kanban_create", `{"title": "x", "project_id": "", "session_id": "s"}`)
	if strings.Contains(errText(result), "unknown parameter") {
		t.Fatal(result)
	}
	if !undeclaredArgs["kanban_create"]["project_id"] {
		t.Fatal(undeclaredArgs)
	}
	result = dispatchKanban(t, "kanban_attach_url", `{"task_id": "x", "url": "https://example.invalid/y.pdf", "title": "spec"}`)
	if strings.Contains(errText(result), "unknown parameter") {
		t.Fatal(result)
	}
}

func TestUnknownParameterErrorListsEveryValidParameterSorted(t *testing.T) {
	result := runKanbanTool(t, "kanban_link", `{"zeta": 1, "alpha": 2, "parent_id": "a"}`)
	want := "kanban_link: unknown parameter(s): alpha, zeta. Valid parameters: board, child_id, parent_id. Nothing changed."
	if errText(result) != want {
		t.Fatal(result)
	}
}

func TestExactArgsRestoresNumberTypesAndKeyOrderFromTheRawJSON(t *testing.T) {
	call := &tools.Call{
		Args:    map[string]any{"limit": 10.0, "ratio": 2.5, "meta": map[string]any{"b": 1.0, "a": 2.0}, "edited": "new", "flag": true, "loose": 3.0},
		RawArgs: `{"limit": 10, "ratio": 2.5, "meta": {"b": 1, "a": 2}, "edited": "old", "flag": 1}`,
	}
	got := exactArgs(call)
	if n, isInt := got["limit"].(int64); !isInt || n != 10 {
		t.Fatalf("limit %#v", got["limit"])
	}
	if got["ratio"] != 2.5 || got["edited"] != "new" || got["flag"] != true || got["loose"] != 3.0 {
		t.Fatalf("args %#v", got)
	}
	meta, isObject := got["meta"].(*pyjson.Object)
	if !isObject || strings.Join(meta.Keys, ",") != "b,a" || meta.Vals["a"] != int64(2) {
		t.Fatalf("meta %#v", got["meta"])
	}
	for _, raw := range []string{"", "not json", "[1, 2]"} {
		kept := exactArgs(&tools.Call{Args: map[string]any{"limit": 10.0}, RawArgs: raw})
		if kept["limit"] != 10.0 {
			t.Fatalf("%q %#v", raw, kept)
		}
	}
	huge := exactArgs(&tools.Call{Args: map[string]any{"priority": 1180591620717411303424.0}, RawArgs: `{"priority": 1180591620717411303424}`})
	if n, isBig := huge["priority"].(*big.Int); !isBig || n.String() != "1180591620717411303424" {
		t.Fatalf("priority %#v", huge["priority"])
	}
}

func TestSqliteBindMatchesTheSQLiteTextAffinity(t *testing.T) {
	for _, c := range []struct {
		value any
		want  string
	}{
		{"text", "text"}, {true, "1"}, {false, "0"}, {int64(42), "42"}, {big.NewInt(-7), "-7"},
		{0.1, "0.1"}, {1.0 / 3, "0.33333333333333332"}, {100.0, "100.0"}, {1e15, "1000000000000000.0"},
		{1e16, "10000000000000000.0"}, {1e17, "1.0e+17"}, {123456.789, "123456.789"}, {5e-324, "4.9406564584124654e-324"},
		{math.Copysign(0, -1), "0.0"}, {2.5e-5, "2.5e-05"}, {math.Inf(-1), "-Inf"}, {1e100, "1.0e+100"},
		{0.30000000000000004, "0.30000000000000004"}, {12345678901234567890.0, "1.2345678901234567e+19"},
	} {
		got, err := sqliteBind(c.value, 1)
		if err != nil || got == nil || *got != c.want {
			t.Fatalf("%#v -> %v %v want %q", c.value, got, err, c.want)
		}
	}
	for _, null := range []any{nil, math.NaN()} {
		if got, err := sqliteBind(null, 1); got != nil || err != nil {
			t.Fatalf("%#v -> %v %v", null, got, err)
		}
	}
	for _, c := range []struct {
		value    any
		position int
		want     string
	}{
		{[]any{1}, 3, "Error binding parameter 3: type 'list' is not supported"},
		{pyjson.ObjectOf("a", 1), 1, "Error binding parameter 1: type 'dict' is not supported"},
		{new(big.Int).Lsh(big.NewInt(1), 70), 6, "Python int too large to convert to SQLite INTEGER"},
	} {
		if _, err := sqliteBind(c.value, c.position); err == nil || err.Error() != c.want {
			t.Fatalf("%#v -> %v", c.value, err)
		}
	}
}

// One wrong typed call of a kanban tool and the Python error text it reports
type wrongTypedCase struct{ tool, args, want string }

// Runs wrong typed cases and asserts each error text exactly
func assertWrongTyped(t *testing.T, cases []wrongTypedCase) {
	t.Helper()
	for _, c := range cases {
		if got := errText(runKanbanTool(t, c.tool, c.args)); got != c.want {
			t.Errorf("%s %s\n got %q\nwant %q", c.tool, c.args, got, c.want)
		}
	}
}

func TestWorkerToolsReportPythonErrorsForWrongTypedArguments(t *testing.T) {
	tid := toolWorkerEnv(t)
	slug := "must be 1-64 chars, lowercase alphanumerics / hyphens / underscores, not starting with '-' or '_'"
	assertWrongTyped(t, []wrongTypedCase{
		{"kanban_complete", `{"summary": "s", "metadata": "x"}`, "metadata must be an object/dict, got str"},
		{"kanban_complete", `{"summary": "s", "created_cards": 5}`, "created_cards must be a list of task ids, got int"},
		{"kanban_complete", `{"summary": "s", "artifacts": {"a": 1}}`, "artifacts must be a list of file paths, got dict"},
		{"kanban_complete", `{"summary": "s", "result": []}`, "kanban_complete: Error binding parameter 1: type 'list' is not supported"},
		{"kanban_block", `{"reason": []}`, "reason is required — explain what input you need"},
		{"kanban_block", `{"reason": "r", "kind": ["x"]}`, "kanban_block: cannot use 'list' as a set element (unhashable type: 'list')"},
		{"kanban_block", `{"reason": "r", "kind": 5}`, "kind must be one of ['capability', 'dependency', 'needs_input', 'transient'] (or omit it)"},
		{"kanban_schedule", `{"reason": 5}`, "reason must be a string"},
		{"kanban_request_review", `{"summary": "s", "metadata": []}`, "metadata must be an object/dict, got list"},
		{"kanban_request_review", `{"summary": "s", "reviewer": 5}`, "reviewer profile '5' is not installed. Installed profiles: default"},
		{"kanban_request_changes", `{"reason": {}}`, "reason is required — describe the changes needed"},
		{"kanban_comment", `{"task_id": 5, "body": "b"}`, "kanban_comment: unknown task 5"},
		{"kanban_comment", `{"task_id": true, "body": "b"}`, "kanban_comment: unknown task True"},
		{"kanban_comment", `{"task_id": [1], "body": "b"}`, "kanban_comment: Error binding parameter 1: type 'list' is not supported"},
		{"kanban_comment", `{"task_id": "x", "body": {}}`, "body is required"},
		{"kanban_attach_url", `{"url": 5}`, "kanban_attach_url: unsupported URL scheme ''; only http/https are allowed"},
		{"kanban_attach", `{"filename": "f", "content_base64": 5}`, "content_base64 is not valid base64: Invalid base64-encoded string: number of data characters (1) cannot be 1 more than a multiple of 4"},
		{"kanban_attach", `{"filename": "f", "content_base64": "not base64!"}`, "content_base64 is not valid base64: Only base64 data is allowed"},
		{"kanban_show", `{"task_id": 5}`, "task 5 not found"},
		{"kanban_show", `{"board": 5.5}`, "kanban_show: invalid board slug 5.5: " + slug},
		{"kanban_show", `{"board": ["x"]}`, "kanban_show: invalid board slug ['x']: " + slug},
		{"kanban_show", `{"board": "Bad Slug"}`, "kanban_show: invalid board slug 'Bad Slug': " + slug},
		{"kanban_link", `{"parent_id": 5, "child_id": "` + tid + `"}`, "kanban_link: sequence item 0: expected str instance, int found"},
		{"kanban_link", `{"parent_id": "t_missing", "child_id": 2.5}`, "kanban_link: sequence item 1: expected str instance, float found"},
		{"kanban_link", `{"parent_id": "t_missing", "child_id": "` + tid + `"}`, "kanban_link: unknown task(s): t_missing"},
		{"kanban_link", `{"parent_id": [1], "child_id": [1]}`, "kanban_link: a task cannot depend on itself"},
		{"kanban_link", `{"parent_id": 1, "child_id": 1.0}`, "kanban_link: a task cannot depend on itself"},
		{"kanban_link", `{"parent_id": "` + tid + `", "child_id": {"a": 1}}`, "kanban_link: Error binding parameter 2: type 'dict' is not supported"},
	})
	if status := boardTask(t, tid).Status; status != "running" {
		t.Fatal(status)
	}
	if comments := boardComments(t, tid); len(comments) != 0 {
		t.Fatal(comments)
	}
}

func TestCompleteKeepsAFalsyNonStringResultAsPythonDoes(t *testing.T) {
	tid := toolWorkerEnv(t)
	if d := runKanbanTool(t, "kanban_complete", `{"summary": "   ", "result": 0}`); d["ok"] != true {
		t.Fatal(d)
	}
	task := boardTask(t, tid)
	if task.Status != "done" || task.Result == nil || *task.Result != "0" {
		t.Fatalf("%q %v", task.Status, task.Result)
	}
	if run := boardLatestRun(t, tid); run.Summary == nil || *run.Summary != "   " {
		t.Fatal(run.Summary)
	}
	var completed *Event
	for _, e := range boardEvents(t, tid) {
		if e.Kind == "completed" {
			completed = e
		}
	}
	if completed == nil || jsonText(t, completed.Payload) != `{"result_len": 0, "summary": null}` {
		t.Fatal(completed)
	}
}

func TestOrchestratorToolsReportPythonErrorsForWrongTypedArguments(t *testing.T) {
	toolWorkerEnv(t)
	unsetEnv(t, "ICEBATH_KANBAN_TASK")
	statuses := "['archived', 'blocked', 'done', 'ready', 'review', 'running', 'scheduled', 'todo', 'triage']"
	assertWrongTyped(t, []wrongTypedCase{
		{"kanban_list", `{"include_archived": "maybe"}`, "include_archived must be a boolean or 'true'/'false'"},
		{"kanban_list", `{"include_archived": 1.0}`, "include_archived must be a boolean or 'true'/'false'"},
		{"kanban_list", `{"limit": "abc"}`, "limit must be an integer"},
		{"kanban_list", `{"limit": [1]}`, "limit must be an integer"},
		{"kanban_list", `{"limit": 0}`, "limit must be >= 1"},
		{"kanban_list", `{"limit": 201}`, "limit must be <= 200"},
		{"kanban_list", `{"status": ["ready"]}`, "kanban_list: cannot use 'list' as a set element (unhashable type: 'list')"},
		{"kanban_list", `{"status": 5}`, "kanban_list: status must be one of " + statuses},
		{"kanban_list", `{"assignee": 5}`, "kanban_list: profile name must be a string, got int: 5"},
		{"kanban_list", `{"tenant": [1]}`, "kanban_list: Error binding parameter 1: type 'list' is not supported"},
		{"kanban_list", `{"assignee": "a", "status": "ready", "tenant": {}}`, "kanban_list: Error binding parameter 3: type 'dict' is not supported"},
		{"kanban_create", `{"title": "t", "assignee": []}`, "assignee is required — name the profile that should execute this task (the dispatcher will only spawn tasks with an assignee)"},
		{"kanban_create", `{"title": "t", "assignee": "a", "triage": "maybe"}`, "triage must be a boolean or 'true'/'false'"},
		{"kanban_create", `{"title": "t", "assignee": "a", "skills": 5}`, "skills must be a list of skill names, got int"},
		{"kanban_create", `{"title": "t", "assignee": "a", "provider": "p"}`, "'provider' requires 'model' to be set as well"},
		{"kanban_create", `{"title": "t", "assignee": "a", "parents": [5]}`, "kanban_create: sequence item 0: expected str instance, int found"},
		{"kanban_create", `{"title": "t", "assignee": "a", "parents": ["t_gone"]}`, "kanban_create: unknown parent task(s): t_gone"},
		{"kanban_create", `{"title": "t", "assignee": "a", "parents": [[1]]}`, "kanban_create: Error binding parameter 1: type 'list' is not supported"},
		{"kanban_create", `{"title": "t", "assignee": "a", "priority": "high", "model": 5}`, "kanban_create: invalid literal for int() with base 10: 'high'"},
		{"kanban_create", `{"title": "t", "assignee": "a", "model": 5}`, "kanban_create: 'int' object has no attribute 'strip'"},
		{"kanban_create", `{"title": "t", "assignee": "a", "model": "  ", "provider": "p"}`, "kanban_create: provider_override requires a model_override"},
		{"kanban_create", `{"title": "t", "assignee": "a", "completion_contract": 5}`, "kanban_create: completion_contract must be local-only, OWNER/REPO, or an exact GitHub PR URL"},
		{"kanban_create", `{"title": "t", "assignee": "a", "workspace_kind": 5}`, "kanban_create: workspace_kind must be one of ['dir', 'scratch', 'worktree'], got 5"},
		{"kanban_create", `{"title": "t", "assignee": "a", "workspace_kind": []}`, "kanban_create: cannot use 'list' as a set element (unhashable type: 'list')"},
		{"kanban_create", `{"title": "t", "assignee": "a", "body": [], "initial_status": "bogus"}`, "kanban_create: initial_status must be one of ['blocked', 'running']"},
		{"kanban_create", `{"title": "t", "assignee": "a", "body": [], "priority": 1180591620717411303424}`, "kanban_create: Error binding parameter 3: type 'list' is not supported"},
		{"kanban_create", `{"title": "t", "assignee": "a", "priority": 1180591620717411303424, "tenant": [1]}`, "kanban_create: Python int too large to convert to SQLite INTEGER"},
		{"kanban_create", `{"title": "t", "assignee": "a", "tenant": [1]}`, "kanban_create: Error binding parameter 13: type 'list' is not supported"},
		{"kanban_create", `{"title": "t", "assignee": "a", "parents": ["t_a", {"k": 1}]}`, "kanban_create: Error binding parameter 2: type 'dict' is not supported"},
		{"kanban_create", `{"title": "t", "assignee": "a", "idempotency_key": [1], "parents": ["t_a", {"k": 1}]}`, "kanban_create: Error binding parameter 1: type 'list' is not supported"},
		{"kanban_create", `{"title": "t", "assignee": "a", "idempotency_key": []}`, "kanban_create: Error binding parameter 14: type 'list' is not supported"},
		{"kanban_create", `{"title": "t", "assignee": "a", "max_runtime_seconds": 1180591620717411303424}`, "kanban_create: Python int too large to convert to SQLite INTEGER"},
		{"kanban_unblock", `{"task_id": 5}`, "could not unblock 5 (not blocked or unknown)"},
	})
	conn := mustConnect(t, "", nil)
	defer conn.Close()
	if titles := taskTitles(t, conn); len(titles) != 1 || titles[0] != "worker-test" {
		t.Fatalf("a refused create left tasks %v", titles)
	}
}

func TestCreateReportsAnInsertBindingFailureOnlyAfterEveryEarlierValidation(t *testing.T) {
	toolWorkerEnv(t)
	unsetEnv(t, "ICEBATH_KANBAN_TASK")
	first := runKanbanTool(t, "kanban_create", `{"title": "keyed", "assignee": "a", "idempotency_key": "k1"}`)
	if first["ok"] != true {
		t.Fatal(first)
	}
	again := runKanbanTool(t, "kanban_create", `{"title": "keyed", "assignee": "a", "idempotency_key": "k1", "body": [], "tenant": [1]}`)
	if again["ok"] != true || again["task_id"] != first["task_id"] {
		t.Fatalf("an idempotency hit never reaches the insert binding: %v", again)
	}
	assertWrongTyped(t, []wrongTypedCase{
		{"kanban_create", `{"title": "t", "assignee": "a", "body": [], "skills": ["web"]}`,
			"kanban_create: 'web' is a toolset name, not skill name(s). Put toolsets in the assignee profile's `toolsets:` config instead of per-task skills. Skills are named skill bundles (e.g. `blogwatcher`, `github-code-review`); toolsets are runtime capabilities (e.g. `web`, `browser`, `terminal`)."},
		{"kanban_create", `{"title": "t", "assignee": "a", "body": [], "parents": ["t_gone"]}`, "kanban_create: unknown parent task(s): t_gone"},
		{"kanban_create", `{"title": "t", "assignee": "Bad Name!", "body": []}`, "kanban_create: Error binding parameter 3: type 'list' is not supported"},
	})
	conn := mustConnect(t, "", nil)
	defer conn.Close()
	if titles := taskTitles(t, conn); len(titles) != 2 {
		t.Fatalf("tasks %v", titles)
	}
}

// Profile home holding a config.yaml with the given body, bound to a tool env
func kanbanConfigEnv(t *testing.T, body string) *tools.Env {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	return &tools.Env{Home: h}
}

func TestProfileHasKanbanToolsetReadsTheSelectionThenTheConfig(t *testing.T) {
	clearKanbanEnv(t)
	for _, c := range []struct {
		name, config string
		selection    []string
		inAssembly   bool
		want         bool
	}{
		{"no opt in", "{}\n", nil, false, false},
		{"profile wide list", "toolsets: [web, kanban]\n", nil, false, true},
		{"profile wide list during assembly", "toolsets: [kanban]\n", []string{"web"}, true, true},
		{"profile wide string is a substring test", "toolsets: my-kanban-bundle\n", nil, false, true},
		{"non iterable toolsets fails closed", "toolsets: 5\nplatform_toolsets:\n  cli: [kanban]\n", nil, false, false},
		{"explicit selection", "{}\n", []string{"terminal", "kanban"}, true, true},
		{"assembly never borrows a platform opt in", "platform_toolsets:\n  cli: [kanban]\n", []string{"terminal"}, true, false},
		{"offer time platform opt in", "platform_toolsets:\n  cli: [kanban]\n", nil, false, true},
		{"platform value that is no list", "platform_toolsets:\n  cli: kanban\n", nil, false, false},
		{"platform_toolsets that is no dict", "platform_toolsets: [kanban]\n", nil, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := kanbanConfigEnv(t, c.config)
			if c.inAssembly {
				env = tools.WithKanbanToolsetSelection(env, c.selection)
			}
			requested, inAssembly := tools.KanbanToolsetRequested(env)
			if inAssembly != c.inAssembly || requested != (c.inAssembly && strings.Contains(strings.Join(c.selection, ","), "kanban")) {
				t.Fatalf("selection %v %v", requested, inAssembly)
			}
			if got := profileHasKanbanToolset(env); got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}

func TestKanbanVisibilitySplitsLifecycleAndOrchestratorTools(t *testing.T) {
	clearKanbanEnv(t)
	env := kanbanConfigEnv(t, "toolsets: [kanban]\n")
	if !checkKanbanMode(env) || !checkKanbanOrchestratorMode(env) {
		t.Fatal("an orchestrator profile with the toolset sees every kanban tool")
	}
	t.Setenv("ICEBATH_KANBAN_TASK", "t_worker")
	if !checkKanbanMode(env) || checkKanbanOrchestratorMode(env) {
		t.Fatal("a dispatcher worker sees lifecycle tools only")
	}
	t.Setenv("ICEBATH_DELEGATED_CHILD_CONTEXT", "1")
	if !checkKanbanMode(env) || !checkKanbanOrchestratorMode(env) {
		t.Fatal("a descendant process falls back to the profile toolset")
	}
	plain := kanbanConfigEnv(t, "{}\n")
	if checkKanbanMode(plain) || checkKanbanOrchestratorMode(plain) {
		t.Fatal("a descendant without the profile toolset sees kanban tools")
	}
	unsetEnv(t, "ICEBATH_DELEGATED_CHILD_CONTEXT")
	prev := delegationCtx
	t.Cleanup(func() { delegationCtx = prev })
	var asked []string
	delegationCtx = func(_ context.Context, _ *tools.Env, predicate string, def bool) bool {
		asked = append(asked, predicate)
		return predicate == "is_delegated_child_context"
	}
	if checkKanbanMode(env) || checkKanbanOrchestratorMode(env) {
		t.Fatal("a delegate child sees kanban tools")
	}
	if strings.Join(asked, ",") != "is_delegated_child_context,is_delegated_child_context" {
		t.Fatal(asked)
	}
	delegationCtx = prev
	if delegationCtx(context.Background(), nil, "no_such_predicate", true) != true || delegationCtx(context.Background(), nil, "no_such_predicate", false) {
		t.Fatal("an unknown predicate must return its default")
	}
}

func TestWorkerGuardRefusesInOrder(t *testing.T) {
	tid := toolWorkerEnv(t)
	k := gateCall(nil)
	k.args = map[string]any{"task_id": "t_other"}
	if _, err := workerGuard(k, "kanban_heartbeat"); err == nil || !strings.Contains(err.Error(), "worker is scoped to task "+tid+"; refusing to mutate t_other.") {
		t.Fatal(err)
	}
	k.args = map[string]any{}
	t.Setenv("ICEBATH_KANBAN_RUN_ID", "")
	if got, err := workerGuard(k, "kanban_heartbeat"); err != nil || got != tid {
		t.Fatal(got, err)
	}
	if _, err := workerGuard(k, "kanban_complete"); err == nil || !strings.Contains(err.Error(), "kanban_complete refused: this worker cannot resolve its ICEBATH_KANBAN_RUN_ID") {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_DELEGATED_CHILD_CONTEXT", "1")
	if _, err := workerGuard(k, "kanban_complete"); err == nil || !strings.Contains(err.Error(), "kanban_complete refused: delegate_task child agents are not Kanban run owners.") {
		t.Fatal(err)
	}
	unsetEnv(t, "ICEBATH_DELEGATED_CHILD_CONTEXT")
	env := &tools.Env{Values: &sync.Map{}}
	defer tools.EnterNonDispatcherOwnedContext(env)()
	if _, err := requireTaskID(gateCall(env)); err == nil || !strings.Contains(err.Error(), "this session does not own the inherited ICEBATH_KANBAN_TASK") {
		t.Fatal(err)
	}
	if err := requireOrchestratorTool("kanban_list"); err == nil || !strings.Contains(err.Error(), "kanban_list is orchestrator-only;") {
		t.Fatal(err)
	}
}
