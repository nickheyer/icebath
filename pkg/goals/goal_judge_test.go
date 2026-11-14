package goals

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Python exception of a named class the fakes raise
type raised struct{ class, msg string }

func (e *raised) Error() string      { return e.msg }
func (e *raised) PyTypeName() string { return e.class }

// Chat completion shaped reply carrying one message content
func judgeReply(content any) map[string]any {
	return map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}}
}

// Answers every auxiliary call with reply or err and records the keyword arguments of the last one
func pinGoalCallLLM(t *testing.T, reply any, err error) *map[string]any {
	t.Helper()
	saved := goalCallLLM
	t.Cleanup(func() { goalCallLLM = saved })
	captured := map[string]any{}
	goalCallLLM = func(ctx context.Context, kwargs map[string]any) (any, error) {
		for k := range captured {
			delete(captured, k)
		}
		for k, v := range kwargs {
			captured[k] = v
		}
		if err != nil {
			return nil, err
		}
		return judgeReply(reply), nil
	}
	return &captured
}

// Content of the user message of the last captured auxiliary call
func capturedUserPrompt(captured *map[string]any) string {
	messages, _ := (*captured)["messages"].([]any)
	for _, m := range messages {
		if msg, ok := m.(map[string]any); ok && msg["role"] == "user" {
			text, _ := msg["content"].(string)
			return text
		}
	}
	return ""
}

// Makes the judge the manager asks answer with a fixed result and counts its calls
func pinGoalJudge(t *testing.T, result JudgeResult) *int {
	t.Helper()
	saved := goalJudgeFn
	t.Cleanup(func() { goalJudgeFn = saved })
	calls := 0
	goalJudgeFn = func(context.Context, string, string, JudgeOptions) JudgeResult {
		calls++
		return result
	}
	return &calls
}

// Serves the given config tree to the goal judge settings
func pinGoalConfig(t *testing.T, cfg config.Tree) {
	t.Helper()
	saved := goalLoadConfig
	t.Cleanup(func() { goalLoadConfig = saved })
	goalLoadConfig = func(context.Context) (config.Tree, error) { return cfg, nil }
}

func pyKind(err error) string {
	var pyErr *pyint.Error
	if errors.As(err, &pyErr) {
		return pyErr.Kind
	}
	return ""
}

// Judge reply parse with the parse error failing the test
func parseReply(t *testing.T, raw any) (string, string, bool, map[string]any) {
	t.Helper()
	verdict, reason, failed, wait, err := parseJudgeResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return verdict, reason, failed, wait
}

func TestParseJudgeResponseCleanJsonDone(t *testing.T) {
	verdict, reason, _, wait := parseReply(t, `{"done": true, "reason": "all good"}`)
	if verdict != "done" || reason != "all good" || wait != nil {
		t.Fatalf("%q %q %v", verdict, reason, wait)
	}
}

func TestParseJudgeResponseWaitVerdictWithPid(t *testing.T) {
	verdict, reason, parseFailed, wait := parseReply(t, `{"verdict": "wait", "wait_on_pid": 4242, "reason": "CI running"}`)
	if verdict != "wait" || parseFailed || !reflect.DeepEqual(wait, map[string]any{"pid": int64(4242)}) || reason != "CI running" {
		t.Fatalf("%q %q %v %v", verdict, reason, parseFailed, wait)
	}
}

func TestParseJudgeResponseCoversEveryReplyShape(t *testing.T) {
	for _, c := range []struct {
		raw, verdict, reason string
		failed               bool
		wait                 map[string]any
	}{
		{"", "continue", "judge returned empty response", true, nil},
		{"I think so", "continue", "judge reply was not JSON: 'I think so'", true, nil},
		{"```json\n{\"verdict\": \"DONE \", \"reason\": \" ok \"}\n```", "done", "ok", false, nil},
		{`noise {"done": "Yes"} trailing`, "done", "no reason provided", false, nil},
		{`{"done": "nope"}`, "continue", "no reason provided", false, nil},
		{`{"verdict": "maybe", "reason": "x"}`, "continue", "x", false, nil},
		{`{"verdict": "wait", "session_id": " proc_1 ", "wait_on_pid": 5, "reason": "r"}`, "wait", "r", false, map[string]any{"session_id": "proc_1"}},
		{`{"verdict": "wait", "wait_on_session": "  ", "wait_on_pid": 5, "reason": "r"}`, "wait", "r", false, map[string]any{"pid": int64(5)}},
		{`{"verdict": "wait", "wait_on_pid": "0", "pid": "77", "reason": "r"}`, "wait", "r", false, map[string]any{"pid": int64(77)}},
		{`{"verdict": "wait", "wait_on_pid": [1], "wait_seconds": 30.9, "reason": "r"}`, "wait", "r", false, map[string]any{"seconds": int64(30)}},
		{`{"verdict": "wait", "reason": "r"}`, "continue", "r (wait verdict had no target — continuing)", false, nil},
		{`[1, 2]`, "continue", "judge reply was not JSON: '[1, 2]'", true, nil},
	} {
		verdict, reason, failed, wait := parseReply(t, c.raw)
		if verdict != c.verdict || reason != c.reason || failed != c.failed || !reflect.DeepEqual(wait, c.wait) {
			t.Errorf("%q gave %q %q %v %v", c.raw, verdict, reason, failed, wait)
		}
	}
	verdict, _, _, wait := parseReply(t, `{"verdict": "wait", "wait_on_pid": 99999999999999999999, "reason": "r"}`)
	if verdict != "wait" || pyrepr.Str(wait["pid"]) != "99999999999999999999" {
		t.Fatalf("huge pid %q %v", verdict, wait)
	}
	if _, _, _, _, err := parseJudgeResponse([]any{"x"}); pyKind(err) != "AttributeError" || err.Error() != "'list' object has no attribute 'strip'" {
		t.Fatalf("list reply error %v", err)
	}
}

func TestJudgeGoalApiErrorContinues(t *testing.T) {
	pinGoalConfig(t, config.Tree{})
	pinGoalCallLLM(t, "", &raised{"RuntimeError", "boom"})
	result := JudgeGoal(context.Background(), "goal", "response", JudgeOptions{})
	if result.Verdict != "continue" || !strings.Contains(strings.ToLower(result.Reason), "judge error") || result.Reason != "judge error: RuntimeError" {
		t.Fatalf("%+v", result)
	}
}

func TestJudgeGoalJudgeSaysDone(t *testing.T) {
	pinGoalConfig(t, config.Tree{})
	pinGoalCallLLM(t, `{"done": true, "reason": "achieved"}`, nil)
	result := JudgeGoal(context.Background(), "goal", "agent response", JudgeOptions{})
	if result.Verdict != "done" || result.Reason != "achieved" {
		t.Fatalf("%+v", result)
	}
	if got := JudgeGoal(context.Background(), "  ", "x", JudgeOptions{}); got.Verdict != "skipped" || got.Reason != "empty goal" {
		t.Fatalf("empty goal %+v", got)
	}
	if got := JudgeGoal(context.Background(), "g", " ", JudgeOptions{}); got.Verdict != "continue" || got.Reason != "empty response (nothing to evaluate)" {
		t.Fatalf("empty response %+v", got)
	}
	pinGoalCallLLM(t, nil, nil)
	if got := JudgeGoal(context.Background(), "g", "r", JudgeOptions{}); got.Verdict != "continue" || !got.ParseFailed || got.Reason != "judge returned empty response" {
		t.Fatalf("no content %+v", got)
	}
	pinGoalCallLLM(t, []any{"block"}, nil)
	if _, err := judgeRaising(context.Background(), "g", "r", JudgeOptions{}); pyKind(err) != "AttributeError" {
		t.Fatalf("list content error %v", err)
	}
}

func TestJudgeParseFailureAutoPauseApiErrorDoesNotCountAsParseFailure(t *testing.T) {
	pinGoalConfig(t, config.Tree{})
	pinGoalCallLLM(t, "", &raised{"RuntimeError", "connection reset"})
	result := JudgeGoal(context.Background(), "goal", "response", JudgeOptions{})
	if result.Verdict != "continue" || result.ParseFailed || !result.TransportFailed {
		t.Fatalf("%+v", result)
	}
}

func TestJudgeGoalWithSubgoalsJudgeUsesSubgoalsTemplateWhenProvided(t *testing.T) {
	goalsTestHome(t)
	pinGoalConfig(t, config.Tree{})
	captured := pinGoalCallLLM(t, `{"done": true, "reason": "all done"}`, nil)
	result := JudgeGoal(context.Background(), "ship the feature", "ok shipped", JudgeOptions{Subgoals: []string{"write tests", "update docs"}})
	prompt := capturedUserPrompt(captured)
	if !strings.Contains(prompt, "Additional criteria") || !strings.Contains(prompt, "1. write tests") || !strings.Contains(prompt, "2. update docs") || result.Verdict != "done" {
		t.Fatalf("verdict %q prompt %q", result.Verdict, prompt)
	}
	if (*captured)["task"] != "goal_judge" || (*captured)["temperature"] != int64(0) || (*captured)["max_tokens"] != int64(DefaultJudgeMaxTokens) {
		t.Fatalf("call arguments %v", *captured)
	}
}

func TestJudgeGoalWithSubgoalsJudgeUsesOriginalTemplateWhenNoSubgoals(t *testing.T) {
	goalsTestHome(t)
	pinGoalConfig(t, config.Tree{})
	captured := pinGoalCallLLM(t, `{"done": true, "reason": "ok"}`, nil)
	JudgeGoal(context.Background(), "ship it", "done", JudgeOptions{Subgoals: nil})
	if prompt := capturedUserPrompt(captured); strings.Contains(prompt, "Additional criteria") || !strings.Contains(prompt, "ship it") {
		t.Fatalf("prompt %q", prompt)
	}
}

func TestJudgeWithContractJudgeUsesContractTemplate(t *testing.T) {
	goalsTestHome(t)
	pinGoalConfig(t, config.Tree{})
	captured := pinGoalCallLLM(t, `{"done": false, "reason": "more"}`, nil)
	JudgeGoal(context.Background(), "ship it", "I think it's done", JudgeOptions{Contract: &GoalContract{Verification: "pytest -q passes"}})
	if prompt := capturedUserPrompt(captured); !strings.Contains(strings.ToLower(prompt), "completion contract") || !strings.Contains(prompt, "pytest -q passes") {
		t.Fatalf("prompt %q", prompt)
	}
}

func TestContractAndBackgroundComposeJudgePromptCarriesContractAndBackground(t *testing.T) {
	goalsTestHome(t)
	pinGoalConfig(t, config.Tree{})
	captured := pinGoalCallLLM(t, `{"verdict": "wait", "wait_on_pid": 4242, "reason": "CI still running"}`, nil)
	background := []map[string]any{{"session_id": "ci-watch", "pid": int64(4242), "status": "running", "command": "wait_for_pr_green.sh 50501", "trigger": "exit"}}
	result := JudgeGoal(context.Background(), "ship the PR", "I pushed and started the CI watcher; waiting on it now.",
		JudgeOptions{Contract: &GoalContract{Verification: "PR CI goes green"}, BackgroundProcesses: background})
	prompt := capturedUserPrompt(captured)
	for _, want := range []string{"PR CI goes green", "Background processes", "4242", "- pid 4242 / session ci-watch: wait_for_pr_green.sh 50501"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q: %q", want, prompt)
		}
	}
	if !strings.Contains(strings.ToLower(prompt), "completion contract") || result.Verdict != "wait" || len(result.Wait) == 0 || result.Wait["pid"] != int64(4242) {
		t.Fatalf("result %+v", result)
	}
}

func TestRenderBackgroundBlockListsOnlyRunningProcessesWithTheirTriggers(t *testing.T) {
	block := renderBackgroundBlock([]map[string]any{
		{"pid": int64(7), "status": "running", "command": "poll.sh\n--fast", "uptime_seconds": int64(12), "watch_patterns": []any{"READY", "DONE"}, "watch_hit": true, "output_preview": "tick\ntock"},
		{"pid": int64(8), "status": "exited", "command": "gone"},
		{"status": "running", "command": "no pid"},
		{"pid": int64(9), "status": "running", "command": strings.Repeat("x", 130), "notify_on_complete": true},
	})
	want7 := "- pid 7: poll.sh --fast (running 12s) | watch_patterns=['READY', 'DONE'] [already matched] | recent output: tick tock"
	want9 := "- pid 9: " + strings.Repeat("x", 120) + "… [truncated] | notify_on_complete"
	if block != "Background processes the agent currently has running (it may be waiting on one of these):\n"+want7+"\n"+want9+"\n\n" {
		t.Fatalf("block %q", block)
	}
	if renderBackgroundBlock(nil) != "" {
		t.Fatal("empty input rendered a block")
	}
}

func TestJudgePromptStatesActiveDelegationsAndAWaitBranchForThem(t *testing.T) {
	pinGoalConfig(t, config.Tree{})
	captured := pinGoalCallLLM(t, `{"verdict": "wait", "wait_for_seconds": 900, "reason": "waiting on workers"}`, nil)
	result := JudgeGoal(context.Background(), "refactor everything", "Waiting on 4 workers; nothing to dispatch.", JudgeOptions{ActiveDelegations: 4})
	text := pyrepr.Str((*captured)["messages"])
	if !strings.Contains(text, "Active delegations") || result.Verdict != "wait" || result.ParseFailed || result.Wait["seconds"] != int64(900) {
		t.Fatalf("result %+v prompt %q", result, text)
	}
	JudgeGoal(context.Background(), "g", "r", JudgeOptions{ActiveDelegations: 0})
	if strings.Contains(pyrepr.Str((*captured)["messages"]), "Active delegations") {
		t.Fatal("delegations block rendered with none active")
	}
}

func TestCountActiveDelegationsIsScopedToTheSpawningSession(t *testing.T) {
	restore := tools.SwapAsyncRecords([]string{"a", "b", "c"}, map[string]map[string]any{
		"a": {"status": "running", "parent_session_id": "root", "session_key": "", "origin_ui_session_id": ""},
		"b": {"status": "completed", "parent_session_id": "root", "session_key": "", "origin_ui_session_id": ""},
		"c": {"status": "running", "parent_session_id": "other", "session_key": "", "origin_ui_session_id": ""},
	})
	defer restore()
	if CountActiveDelegations("root") != 1 || CountActiveDelegations("other") != 1 || CountActiveDelegations("") != 0 {
		t.Fatalf("counts %d %d %d", CountActiveDelegations("root"), CountActiveDelegations("other"), CountActiveDelegations(""))
	}
}

func TestReaderReturnsConfiguredTimeout(t *testing.T) {
	pinGoalConfig(t, config.Tree{"auxiliary": map[string]any{"goal_judge": map[string]any{"timeout": int64(120)}}})
	if got := goalJudgeTimeout(context.Background()); got != 120.0 {
		t.Fatalf("timeout %v", got)
	}
}

func TestReaderFallsBackWhenKeyAbsent(t *testing.T) {
	pinGoalConfig(t, config.Tree{"auxiliary": map[string]any{"goal_judge": map[string]any{"max_tokens": int64(512)}}})
	if got := goalJudgeTimeout(context.Background()); got != DefaultJudgeTimeout {
		t.Fatalf("timeout %v", got)
	}
	if got := goalJudgeMaxTokens(context.Background()); got != int64(512) {
		t.Fatalf("max tokens %v", got)
	}
}

func TestReaderFallsBackOnNonPositiveOrGarbage(t *testing.T) {
	for _, bad := range []any{int64(0), int64(-5), "fast", nil} {
		pinGoalConfig(t, config.Tree{"auxiliary": map[string]any{"goal_judge": map[string]any{"timeout": bad, "max_tokens": bad}}})
		if got := goalJudgeTimeout(context.Background()); got != DefaultJudgeTimeout {
			t.Fatalf("timeout for %v gave %v", bad, got)
		}
		if got := goalJudgeMaxTokens(context.Background()); got != int64(DefaultJudgeMaxTokens) {
			t.Fatalf("max tokens for %v gave %v", bad, got)
		}
	}
	for _, cfg := range []config.Tree{{}, {"auxiliary": "x"}, {"auxiliary": map[string]any{"goal_judge": nil}}} {
		pinGoalConfig(t, cfg)
		if got := goalJudgeTimeout(context.Background()); got != DefaultJudgeTimeout {
			t.Fatalf("timeout for %v gave %v", cfg, got)
		}
	}
}

func TestJudgeGoalResolvesTimeoutFromConfig(t *testing.T) {
	pinGoalConfig(t, config.Tree{"auxiliary": map[string]any{"goal_judge": map[string]any{"timeout": int64(120)}}})
	captured := pinGoalCallLLM(t, "", &raised{"TimeoutError", "simulated transport timeout"})
	result := JudgeGoal(context.Background(), "do the thing", "here is a substantive response to evaluate", JudgeOptions{})
	if (*captured)["timeout"] != 120.0 || result.Verdict != "continue" || !result.TransportFailed {
		t.Fatalf("timeout %v result %+v", (*captured)["timeout"], result)
	}
}

func TestJudgeGoalExplicitTimeoutWins(t *testing.T) {
	pinGoalConfig(t, config.Tree{"auxiliary": map[string]any{"goal_judge": map[string]any{"timeout": int64(120)}}})
	captured := pinGoalCallLLM(t, "", &raised{"TimeoutError", "simulated transport timeout"})
	explicit := 7.5
	JudgeGoal(context.Background(), "g", "r", JudgeOptions{Timeout: &explicit})
	if (*captured)["timeout"] != 7.5 {
		t.Fatalf("timeout %v", (*captured)["timeout"])
	}
}

func TestParseContractInlineFieldsParsed(t *testing.T) {
	headline, contract := ParseContract("Migrate auth to JWT\nverify: the auth test suite passes\nconstraints: keep the /login response shape unchanged\n" +
		"boundaries: only touch services/auth and its tests\nstop when: a schema change needs product sign-off")
	if headline != "Migrate auth to JWT" || contract.Verification != "the auth test suite passes" || contract.Constraints != "keep the /login response shape unchanged" ||
		contract.Boundaries != "only touch services/auth and its tests" || contract.StopWhen != "a schema change needs product sign-off" || contract.IsEmpty() {
		t.Fatalf("%q %+v", headline, contract)
	}
}

func TestParseContractAliasVariants(t *testing.T) {
	_, contract := ParseContract("Goal\nverified by: tests green\npreserve: public API")
	if contract.Verification != "tests green" || contract.Constraints != "public API" {
		t.Fatalf("%+v", contract)
	}
	headline, contract := ParseContract("Ship: the thing\n\n  DONE WHEN:  it works \nverify:\nverify: a\nVerify: b")
	if headline != "Ship: the thing verify:" || contract.Outcome != "it works" || contract.Verification != "a b" {
		t.Fatalf("%q %+v", headline, contract)
	}
	if headline, contract := ParseContract(""); headline != "" || !contract.IsEmpty() {
		t.Fatal("empty text parsed into a contract")
	}
	if got := renderExtraCriteria([]string{"a", "b"}); got != "- Extra criterion 1: a\n- Extra criterion 2: b" {
		t.Fatalf("extra criteria %q", got)
	}
}

func TestDraftContractDraftParsesJson(t *testing.T) {
	goalsTestHome(t)
	pinGoalConfig(t, config.Tree{})
	captured := pinGoalCallLLM(t, `{"outcome": "auth on JWT", "verification": "auth suite green", "constraints": "no API change", `+
		`"boundaries": "services/auth", "stop_when": "schema change needed"}`, nil)
	contract := DraftContract(context.Background(), "Migrate auth to JWT", nil)
	if contract == nil || contract.Outcome != "auth on JWT" || contract.Verification != "auth suite green" || contract.IsEmpty() {
		t.Fatalf("contract %+v", contract)
	}
	if capturedUserPrompt(captured) != "Objective:\nMigrate auth to JWT" {
		t.Fatalf("prompt %q", capturedUserPrompt(captured))
	}
}

func TestDraftContractDraftReturnsNoneWhenNoClient(t *testing.T) {
	goalsTestHome(t)
	pinGoalConfig(t, config.Tree{})
	pinGoalCallLLM(t, "", &raised{"RuntimeError", "No LLM provider configured"})
	if DraftContract(context.Background(), "anything", nil) != nil {
		t.Fatal("contract drafted with no client")
	}
	pinGoalCallLLM(t, "not json", nil)
	if DraftContract(context.Background(), "anything", nil) != nil || DraftContract(context.Background(), "  ", nil) != nil {
		t.Fatal("contract drafted from an unusable reply or an empty objective")
	}
	pinGoalCallLLM(t, `{"outcome": " "}`, nil)
	if DraftContract(context.Background(), "anything", nil) != nil {
		t.Fatal("an empty contract was returned")
	}
}

func TestBlockedVerdictParseJudgeResponseAcceptsBlocked(t *testing.T) {
	verdict, reason, parseFailed, _ := parseReply(t, `{"verdict": "blocked", "reason": "the repo was deleted"}`)
	if verdict != "blocked" || reason != "the repo was deleted" || parseFailed {
		t.Fatalf("%q %q %v", verdict, reason, parseFailed)
	}
}

func TestGatherBackgroundProcessesOwnershipOnlyTheOwningSessionsProcessesAreSeen(t *testing.T) {
	saved := goalListSessions
	t.Cleanup(func() { goalListSessions = saved })
	goalListSessions = func(context.Context, string) []map[string]any {
		return []map[string]any{
			{"session_id": "proc_mine", "status": "running", "owner_task_id": "root-sid", "task_id": "default"},
			{"session_id": "proc_child", "status": "running", "owner_task_id": "sa-1-abc", "task_id": "default"},
			{"session_id": "proc_done", "status": "exited", "owner_task_id": "root-sid", "task_id": "default"},
		}
	}
	ids := func(rows []map[string]any) []string {
		out := []string{}
		for _, row := range rows {
			out = append(out, row["session_id"].(string))
		}
		return out
	}
	ctx := context.Background()
	if got := ids(GatherBackgroundProcesses(ctx, "", "")); !reflect.DeepEqual(got, []string{"proc_mine", "proc_child"}) {
		t.Fatalf("unfiltered %v", got)
	}
	if got := ids(GatherBackgroundProcesses(ctx, "", "root-sid")); !reflect.DeepEqual(got, []string{"proc_mine"}) {
		t.Fatalf("owner filtered %v", got)
	}
	goalListSessions = func(context.Context, string) []map[string]any { panic("registry torn down") }
	if got := GatherBackgroundProcesses(ctx, "", ""); got == nil || len(got) != 0 {
		t.Fatalf("failing registry gave %v", got)
	}
}

func TestGatewayAndTuiSurfacesUseTheSameRule(t *testing.T) {
	longGoal := "HANDOFF " + strings.Repeat("step; ", 120)
	if GoalKickPrompt(longGoal, longGoal) != GoalAlreadySeenKick {
		t.Fatal("a goal the user just sent was kicked verbatim")
	}
	if got := GoalKickPrompt("ship the API", strings.Repeat("ship the API or ship the UI? ", 8)); got != "ship the API" {
		t.Fatalf("short goal kicked as %q", got)
	}
	if LastUserMessageFromDB(context.Background(), "") != "" {
		t.Fatal("a message was read for no session")
	}
	if got := GoalKickPrompt(longGoal, []any{map[string]any{"type": "text", "text": longGoal}, "skip"}); got != GoalAlreadySeenKick {
		t.Fatal("a block list message was not recognised")
	}
	history := []map[string]any{{"role": "user", "content": "first"}, {"role": "assistant", "content": "a"}, {"role": "user", "content": "second"}, {"role": "tool", "content": "t"}}
	if LastUserMessageContent(history) != "second" || LastUserMessageContent([]map[string]any{{"role": "assistant"}}) != "" || LastUserMessageContent(nil) != "" {
		t.Fatal("newest user message")
	}
}

func TestLastUserMessageFromDBReadsTheNewestUserMessageOfTheSession(t *testing.T) {
	goalsTestHome(t)
	ctx := context.Background()
	db := SessionDB(ctx)
	if db == nil {
		t.Fatal("no session store")
	}
	if err := db.CreateSession(state.NewSession{ID: "kick-db", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range [][2]string{{"user", "older"}, {"assistant", "reply"}, {"user", "newest ask"}, {"assistant", "ok"}} {
		if _, err := db.AppendMessage(ctx, "kick-db", map[string]any{"role": m[0], "content": m[1]}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := LastUserMessageFromDB(ctx, "kick-db"); got != "newest ask" {
		t.Fatalf("newest user message %v", got)
	}
	if got := LastUserMessageFromDB(ctx, "absent-session"); got != "" {
		t.Fatalf("message of an absent session %v", got)
	}
}

// Gate run with an interrupt failing the test
func runGate(t *testing.T, gate GoalGate, cwd string) (bool, int64, string) {
	t.Helper()
	passed, code, out, err := RunGate(context.Background(), gate, cwd)
	if err != nil {
		t.Fatal(err)
	}
	return passed, code, out
}

func TestRunGatePass(t *testing.T) {
	passed, code, out := runGate(t, NewGoalGate("echo hello"), "")
	if !passed || code != 0 || !strings.Contains(out, "hello") {
		t.Fatalf("%v %d %q", passed, code, out)
	}
}

func TestRunGateFailCapturesOutput(t *testing.T) {
	passed, code, out := runGate(t, NewGoalGate("echo broken >&2; exit 3"), "")
	if passed || code != 3 || !strings.Contains(out, "broken") || out != "\nbroken\n" {
		t.Fatalf("%v %d %q", passed, code, out)
	}
}

func TestRunGateTimeout(t *testing.T) {
	gate := NewGoalGate("sleep 5")
	gate.TimeoutSeconds = 1
	passed, code, out := runGate(t, gate, "")
	if passed || code != -1 || !strings.Contains(out, "timed out") || out != "\n[gate timed out after 1s]" {
		t.Fatalf("%v %d %q", passed, code, out)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := RunGate(ctx, gate, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupt %v", err)
	}
}

func TestRunGateKeepsDiagnosticsWhenAByteWillNotDecode(t *testing.T) {
	script := filepath.Join(t.TempDir(), "gate.sh")
	if err := os.WriteFile(script, []byte("printf 'FAILED: 3 tests broken \\220\\215 rerun me\\n'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	passed, code, out := runGate(t, NewGoalGate(`sh "`+script+`"`), "")
	if passed || code != 1 || !strings.Contains(out, "FAILED: 3 tests broken") || !strings.Contains(out, "�� rerun me") {
		t.Fatalf("%v %d %q", passed, code, out)
	}
}

func TestRunGateReportsACommandThatCannotStartAndBoundsItsTail(t *testing.T) {
	passed, code, out := runGate(t, NewGoalGate("true"), filepath.Join(t.TempDir(), "missing-dir"))
	if passed || code != -1 || !strings.HasPrefix(out, "[gate could not run: FileNotFoundError: ") {
		t.Fatalf("%v %d %q", passed, code, out)
	}
	passed, code, out = runGate(t, NewGoalGate("head -c 5000 /dev/zero | tr '\\0' 'a'; echo END"), "")
	if !passed || code != 0 || len(out) != gateOutputTailChars || !strings.HasSuffix(out, "aEND\n") {
		t.Fatalf("%v %d tail of %d chars", passed, code, len(out))
	}
}

// Context whose tool environment declares the given session workspace
func sessionWorkspaceCtx(t *testing.T, cwd string) context.Context {
	t.Helper()
	env := &tools.Env{Values: &sync.Map{}}
	if cwd != "" {
		tools.SetLogicalSessionCwd(env, cwd)
	}
	return tools.WithToolEnv(context.Background(), env)
}

func TestGateWorkspaceFollowsTheDeclaredSessionWorkspace(t *testing.T) {
	session := t.TempDir()
	if cwd, refusal := gateWorkspace(sessionWorkspaceCtx(t, session)); cwd != session || refusal != "" {
		t.Fatalf("%q %q", cwd, refusal)
	}
	missing := filepath.Join(session, "gone")
	cwd, refusal := gateWorkspace(sessionWorkspaceCtx(t, missing))
	if cwd != "" || refusal != "the session workspace "+missing+" is not a directory on this host, and running gates anywhere else would check a different project" {
		t.Fatalf("%q %q", cwd, refusal)
	}
	launch := t.TempDir()
	t.Chdir(launch)
	t.Setenv("TERMINAL_CWD", "")
	os.Unsetenv("TERMINAL_CWD")
	resolved, _ := filepath.EvalSymlinks(launch)
	if cwd, refusal := gateWorkspace(sessionWorkspaceCtx(t, "")); refusal != "" || (cwd != launch && cwd != resolved) {
		t.Fatalf("launch directory %q %q want %q", cwd, refusal, launch)
	}
}

func TestGoalPidAliveAndTruncate(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if !goalPidAlive(int64(cmd.Process.Pid)) {
		t.Fatal("a live child read as dead")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if goalPidAlive(0) || goalPidAlive(-4) || goalPidAlive(2_000_000_000) {
		t.Fatal("an impossible pid read as alive")
	}
	if goalTruncate("", 5) != "" || goalTruncate("abcde", 5) != "abcde" || goalTruncate("abcdef", 5) != "abcde… [truncated]" || goalTruncate("ééééé!", 5) != "ééééé… [truncated]" {
		t.Fatal("truncate")
	}
	if goalSessionWaiting("") || CountActiveDelegations("") != 0 {
		t.Fatal("an empty id waited or counted")
	}
}

func TestGoalJudgeReasonNamesNousAuthFailureAndStillFailsOpen(t *testing.T) {
	provider.ClearNousCredentialFailure()
	t.Cleanup(provider.ClearNousCredentialFailure)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	ClearSessionDBCache()
	cfg := "model:\n  provider: nous\n  default: test-model\nauxiliary:\n  goal_judge:\n    provider: nous\n    model: test-model\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	timeout := 5.0
	result := JudgeGoal(context.Background(), "ship the fix", "edited the file and ran the tests", JudgeOptions{Timeout: &timeout})
	if result.Verdict != "continue" || result.ParseFailed || result.Wait != nil || !result.TransportFailed {
		t.Fatalf("result %+v", result)
	}
	if !strings.HasPrefix(result.Reason, "goal_judge auxiliary client unavailable: Nous Portal runtime credentials unavailable:") ||
		!strings.Contains(result.Reason, "icebath model") || strings.Contains(result.Reason, "judge error") {
		t.Fatalf("reason %q", result.Reason)
	}
}
