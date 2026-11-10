//go:build !windows

package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Production allowlist writer, restored by tests that swap in a failing one
var defaultShellHookAtomicWrite = shellHookAtomicWrite

// Isolated home with a fresh manager, cleared shell hook state and a non TTY stdin
func shellHookWorld(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	for _, name := range []string{"ICEBATH_ACCEPT_HOOKS", "ICEBATH_SAFE_MODE", "ICEBATH_KANBAN_TASK", "ICEBATH_DELEGATED_CHILD_CONTEXT"} {
		t.Setenv(name, "")
	}
	ResetPluginManagersForTests()
	ResetShellHooksForTests()
	savedTTY, savedRead, savedOut, savedWrite := shellHookStdinIsTTY, shellHookReadLine, shellHookStdout, shellHookAtomicWrite
	shellHookStdinIsTTY = func() bool { return false }
	shellHookReadLine = func() (string, error) { t.Fatal("prompted without a terminal"); return "", nil }
	shellHookStdout = &strings.Builder{}
	savedServices := installedServices()
	InstallServices(&Services{InvokeLifecycleHook: shellHookLifecycle})
	t.Cleanup(func() {
		InstallServices(savedServices)
		shellHookStdinIsTTY, shellHookReadLine, shellHookStdout, shellHookAtomicWrite = savedTTY, savedRead, savedOut, savedWrite
		ResetShellHooksForTests()
		ResetPluginManagersForTests()
	})
	return h
}

// Terminal stdin answering each prompt from answers and recording how often it was asked
func shellHookTTY(t *testing.T, answers ...string) *int {
	asked := 0
	shellHookStdinIsTTY = func() bool { return true }
	shellHookReadLine = func() (string, error) {
		if asked >= len(answers) {
			t.Fatal("prompted more often than answered")
		}
		asked++
		return answers[asked-1], nil
	}
	return &asked
}

// Lifecycle hook service the top level wiring installs, the current home's manager dispatch
func shellHookLifecycle(ctx context.Context, hook string, kw map[string]any) []any {
	return GetPluginManager(ctx).InvokeHook(ctx, hook, kw)
}

func writeHookScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func hooksCfg(event string, entries ...map[string]any) map[string]any {
	list := []any{}
	for _, e := range entries {
		list = append(list, e)
	}
	return map[string]any{"hooks": map[string]any{event: list}}
}

func wiredShellHooks(event string) []string {
	m := GetPluginManager(context.Background())
	out := []string{}
	for _, cb := range m.hookCallbacks(event) {
		out = append(out, cb.name)
	}
	sort.Strings(out)
	return out
}

func spawnResult(code int, stdout, stderr string) *ShellHookResult {
	return &ShellHookResult{ReturnCode: &code, Stdout: stdout, Stderr: stderr, ElapsedSeconds: 0.1}
}

func TestShellHookParseResponse(t *testing.T) {
	if r := parseShellHookResponse("pre_tool_call", `{"decision": "block", "reason": "nope"}`); !reflect.DeepEqual(r, map[string]any{"action": "block", "message": "nope"}) {
		t.Fatalf("claude code block = %v", r)
	}
	cases := []struct {
		stdout string
		want   map[string]any
	}{
		{`{"action": "approve", "message": "  needs a human ", "rule_key": " terminal:rm "}`, map[string]any{"action": "approve", "message": "needs a human", "rule_key": "terminal:rm"}},
		{`{"action": "approve", "message": "", "rule_key": 7}`, map[string]any{"action": "approve"}},
		{`{"decision": "approve", "reason": "ok"}`, nil},
		{`{"action": "approve", "decision": "block", "reason": "no"}`, map[string]any{"action": "block", "message": "no"}},
	}
	for _, c := range cases {
		if got := parseShellHookResponse("pre_tool_call", c.stdout); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s parsed %v, want %v", c.stdout, got, c.want)
		}
	}
	for _, empty := range []string{"", "   "} {
		if r := parseShellHookResponse("pre_tool_call", empty); r != nil {
			t.Fatalf("empty stdout parsed %v", r)
		}
	}
}

func TestShellHookPreLLMCallBlockInBothDialects(t *testing.T) {
	cases := []struct {
		stdout string
		want   map[string]any
	}{
		{`{"action": "block", "message": "no"}`, map[string]any{"action": "block", "message": "no"}},
		{`{"decision": "block", "reason": "no"}`, map[string]any{"action": "block", "message": "no"}},
		{`{"decision": "block"}`, map[string]any{"action": "block", "message": shellHookDefaultBlockMessage}},
		{`{"context": "extra"}`, map[string]any{"context": "extra"}},
		{`{"action": "allow"}`, nil},
	}
	for _, c := range cases {
		if got := parseShellHookResponse("pre_llm_call", c.stdout); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s parsed %v, want %v", c.stdout, got, c.want)
		}
	}
	if got := parseShellHookResponse("pre_verify", `{"decision": "BLOCK", "reason": " again "}`); !reflect.DeepEqual(got, map[string]any{"action": "continue", "message": "again"}) {
		t.Fatalf("pre_verify parsed %v", got)
	}
}

func TestShellHookSerializePayloadArgsNotDictBecomesNull(t *testing.T) {
	shellHookWorld(t)
	raw, err := serializeShellHookPayload(context.Background(), "pre_tool_call", map[string]any{"args": []any{"not", "a", "dict"}})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	if v, ok := payload["tool_input"]; !ok || v != nil {
		t.Fatalf("tool_input = %v", v)
	}
	if !strings.HasPrefix(raw, `{"hook_event_name": "pre_tool_call", "tool_name": null, "tool_input": null, "session_id": "", "cwd": `) {
		t.Fatalf("wire order %s", raw)
	}
}

func TestShellHookMatcher(t *testing.T) {
	spec := NewShellHookSpec("pre_tool_call", "echo", "terminal|file", 60, false)
	for name, want := range map[string]bool{"terminal": true, "file": true, "web": false} {
		if got, _ := spec.MatchesTool(name); got != want {
			t.Fatalf("matches %s = %v", name, got)
		}
	}
	spec = NewShellHookSpec("pre_tool_call", "echo", " terminal ", 60, false)
	if ok, _ := spec.MatchesTool("terminal"); spec.Matcher != "terminal" || !ok {
		t.Fatalf("stripped matcher %q", spec.Matcher)
	}
	spec = NewShellHookSpec("pre_tool_call", "echo", "   ", 60, false)
	if ok, _ := spec.MatchesTool("anything"); spec.Matcher != "" || !ok {
		t.Fatal("whitespace matcher is not absent")
	}
	logs := captureLogs(t)
	spec = NewShellHookSpec("pre_tool_call", "echo", "term(", 60, false)
	if ok, _ := spec.MatchesTool("term("); !ok || !strings.Contains(logs.String(), "treating as literal equality") {
		t.Fatal("invalid matcher did not fall back to literal equality")
	}
	if ok, _ := spec.MatchesTool(nil); ok {
		t.Fatal("absent tool name matched a matcher")
	}
}

func TestShellHookBlockAndApproveThroughThePluginManager(t *testing.T) {
	h := shellHookWorld(t)
	t.Setenv("ICEBATH_ACCEPT_HOOKS", "1")
	dir := filepath.Dir(h)
	block := writeHookScript(t, dir, "block.sh", "#!/usr/bin/env bash\nprintf '{\"decision\": \"block\", \"reason\": \"blocked-by-shell\"}\\n'\n")
	ctx := context.Background()
	registered, err := RegisterShellHooksFromConfig(ctx, hooksCfg("pre_tool_call", map[string]any{"matcher": "terminal", "command": block}), true, false)
	if err != nil || len(registered) != 1 {
		t.Fatalf("registered %v, %v", registered, err)
	}
	if msg, err := GetPreToolCallBlockMessage(ctx, "terminal", map[string]any{"command": "rm"}, PreToolCallIDs{}); err != nil || msg != "blocked-by-shell" {
		t.Fatalf("block message %q, %v", msg, err)
	}
	ResetShellHooksForTests()
	ResetPluginManagersForTests()
	approve := writeHookScript(t, dir, "approve.sh", "#!/usr/bin/env bash\nprintf '{\"action\": \"approve\", \"message\": \"risky\", \"rule_key\": \"terminal:rm\"}\\n'\n")
	if r, _ := RegisterShellHooksFromConfig(ctx, hooksCfg("pre_tool_call", map[string]any{"matcher": "terminal", "command": approve}), true, false); len(r) != 1 {
		t.Fatalf("approve hook registered %v", r)
	}
	var seen [][3]string
	InstallServices(&Services{InvokeLifecycleHook: shellHookLifecycle, RequestToolApproval: func(ctx context.Context, toolName, message, ruleKey, turnID, toolCallID, sessionID string) (map[string]any, error) {
		seen = append(seen, [3]string{toolName, message, ruleKey})
		return map[string]any{"approved": false, "message": "denied by human"}, nil
	}})
	if msg, err := ResolvePreToolBlock(ctx, "terminal", map[string]any{"command": "rm"}, PreToolCallIDs{}); err != nil || msg != "denied by human" {
		t.Fatalf("resolved %q, %v", msg, err)
	}
	if !reflect.DeepEqual(seen, [][3]string{{"terminal", "risky", "terminal:rm"}}) {
		t.Fatalf("gate saw %v", seen)
	}
}

func TestShellHookCallbackSubprocess(t *testing.T) {
	h := shellHookWorld(t)
	dir, ctx := filepath.Dir(h), context.Background()
	calls := filepath.Join(dir, "calls.log")
	logScript := writeHookScript(t, dir, "log.sh", fmt.Sprintf("#!/usr/bin/env bash\necho \"$(cat -)\" >> %s\nprintf '{}\\n'\n", calls))
	cb := makeShellHookCallback(NewShellHookSpec("pre_tool_call", logScript, "terminal", 60, false))
	for _, call := range []map[string]any{{"tool_name": "terminal", "args": map[string]any{"command": "ls"}},
		{"tool_name": "web_search", "args": map[string]any{"q": "x"}}, {"tool_name": "file_read", "args": map[string]any{"path": "x"}}} {
		if _, err := cb(ctx, call); err != nil {
			t.Fatal(err)
		}
	}
	if raw, _ := os.ReadFile(calls); strings.Count(string(raw), "pre_tool_call") != 1 {
		t.Fatalf("matcher let through %q", raw)
	}
	capture := filepath.Join(dir, "payload.json")
	capScript := writeHookScript(t, dir, "capture.sh", fmt.Sprintf("#!/usr/bin/env bash\ncat - > %s\nprintf '{}\\n'\n", capture))
	cb = makeShellHookCallback(NewShellHookSpec("pre_tool_call", capScript, "", 60, false))
	if _, err := cb(ctx, map[string]any{"tool_name": "terminal", "args": map[string]any{"command": "echo hi"}, "session_id": "sess-77", "task_id": "task-77"}); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	raw, _ := os.ReadFile(capture)
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	extra, _ := payload["extra"].(map[string]any)
	if payload["hook_event_name"] != "pre_tool_call" || payload["tool_name"] != "terminal" || payload["session_id"] != "sess-77" ||
		!reflect.DeepEqual(payload["tool_input"], map[string]any{"command": "echo hi"}) || extra["task_id"] != "task-77" {
		t.Fatalf("payload %v", payload)
	}
	if _, ok := payload["cwd"]; !ok {
		t.Fatal("payload lacks cwd")
	}
	for _, c := range []struct{ body, want string }{
		{`printf '{"action": "modify", "args": {"path": "/safe"}}\n'`, "path"},
		{`printf '{"decision": "modify", "tool_input": {"content": "safe"}}\n'`, "content"},
	} {
		script := writeHookScript(t, dir, "mod.sh", "#!/usr/bin/env bash\n"+c.body)
		got, err := makeShellHookCallback(NewShellHookSpec("pre_tool_call", script, "", 60, false))(ctx, map[string]any{"tool_name": "write_file", "args": map[string]any{c.want: "danger"}})
		args, _ := got.(map[string]any)["args"].(map[string]any)
		if err != nil || got.(map[string]any)["action"] != "modify" || args[c.want] == "danger" || len(args) != 1 {
			t.Fatalf("modify %v, %v", got, err)
		}
	}
}

func TestShellHookParseHooksBlock(t *testing.T) {
	specs := parseShellHooksBlock(map[string]any{"pre_tool_call": []any{map[string]any{"matcher": "terminal", "command": "/tmp/hook.sh", "timeout": 30}}})
	if len(specs) != 1 || specs[0].Event != "pre_tool_call" || specs[0].Matcher != "terminal" || specs[0].Command != "/tmp/hook.sh" || specs[0].Timeout != 30 {
		t.Fatalf("valid entry %+v", specs)
	}
	if specs := parseShellHooksBlock(map[string]any{"transform_api_error_classification": []any{map[string]any{"command": "/tmp/hook.sh"}}}); len(specs) != 0 {
		t.Fatal("python only event registered")
	}
	if specs := parseShellHooksBlock(map[string]any{"post_tool_call": []any{map[string]any{"command": "/tmp/slow.sh", "timeout": 9999}}}); specs[0].Timeout != ShellHookMaxTimeoutSeconds {
		t.Fatalf("timeout %d", specs[0].Timeout)
	}
	for _, v := range []any{nil, "string", []any{}} {
		if specs := parseShellHooksBlock(v); len(specs) != 0 {
			t.Fatalf("hooks %v parsed %v", v, specs)
		}
	}
	if specs := parseShellHooksBlock(map[string]any{"pre_llm_call": []any{map[string]any{"matcher": "terminal", "command": "/bin/echo"}}}); len(specs) != 1 || specs[0].Matcher != "" {
		t.Fatal("non tool event kept its matcher")
	}
	logs := captureLogs(t)
	if _, complete := parseShellHooks(map[string]any{"pre_tool_cal": []any{}}); complete || !strings.Contains(logs.String(), "did you mean 'pre_tool_call'?") {
		t.Fatalf("typo not suggested: %s", logs.String())
	}
	failCases := []struct {
		event string
		entry map[string]any
		want  bool
	}{
		{"pre_tool_call", map[string]any{"command": "/tmp/h.sh", "fail_closed": true}, true},
		{"pre_tool_call", map[string]any{"command": "/tmp/h.sh"}, false},
		{"pre_tool_call", map[string]any{"command": "/tmp/h.sh", "failClosed": true}, true},
		{"pre_tool_call", map[string]any{"command": "/tmp/h.sh", "fail_closed": false, "failClosed": true}, false},
		{"pre_tool_call", map[string]any{"command": "/tmp/h.sh", "fail_closed": "yes"}, false},
		{"on_session_start", map[string]any{"command": "/tmp/h.sh", "fail_closed": true}, false},
	}
	for _, c := range failCases {
		specs := parseShellHooksBlock(map[string]any{c.event: []any{c.entry}})
		if len(specs) != 1 || specs[0].FailClosed != c.want {
			t.Fatalf("%v fail_closed %+v", c.entry, specs)
		}
	}
}

func TestShellHookIdempotentRegistration(t *testing.T) {
	h := shellHookWorld(t)
	t.Setenv("ICEBATH_ACCEPT_HOOKS", "1")
	ctx := context.Background()
	script := writeHookScript(t, filepath.Dir(h), "h.sh", "#!/usr/bin/env bash\nprintf '{}\\n'\n")
	cfg := hooksCfg("on_session_start", map[string]any{"command": script})
	first, _ := RegisterShellHooksFromConfig(ctx, cfg, true, false)
	second, _ := RegisterShellHooksFromConfig(ctx, cfg, true, false)
	if len(first) != 1 || len(second) != 0 || len(wiredShellHooks("on_session_start")) != 1 {
		t.Fatalf("first %d second %d wired %v", len(first), len(second), wiredShellHooks("on_session_start"))
	}
	cfg = hooksCfg("pre_tool_call", map[string]any{"matcher": "terminal", "command": script}, map[string]any{"matcher": "web_search", "command": script})
	if r, _ := RegisterShellHooksFromConfig(ctx, cfg, true, false); len(r) != 2 || len(wiredShellHooks("pre_tool_call")) != 2 {
		t.Fatalf("matcher keyed registration %d", len(r))
	}
}

func TestShellHookAllowlistConcurrentApprovalsAllLand(t *testing.T) {
	shellHookWorld(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := recordShellHookApproval(ctx, "pre_tool_call", fmt.Sprintf("/hooks/%d.sh", i)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if n := len(AllowlistedShellHookPairs(ctx)); n != 8 {
		t.Fatalf("approvals kept %d of 8", n)
	}
	if fi, _ := os.Stat(ShellHookAllowlistPath(ctx)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("allowlist mode %o", fi.Mode().Perm())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(ShellHookAllowlistPath(ctx)), ".shell-hooks-allowlist_*")); len(left) != 0 {
		t.Fatalf("temp files left %v", left)
	}
}

func TestShellHookEvaluateResult(t *testing.T) {
	spec := func(event string, failClosed bool) *ShellHookSpec {
		return NewShellHookSpec(event, "/tmp/h.sh", "", ShellHookDefaultTimeoutSeconds, failClosed)
	}
	pre, preFC := spec("pre_tool_call", false), spec("pre_tool_call", true)
	block := func(m string) map[string]any { return map[string]any{"action": "block", "message": m} }
	errored := &ShellHookResult{Error: "No such file"}
	timedOut := &ShellHookResult{TimedOut: true}
	cases := []struct {
		spec *ShellHookSpec
		r    *ShellHookResult
		want map[string]any
		has  string
	}{
		{pre, spawnResult(2, "", "policy violation\n"), block("policy violation"), ""},
		{pre, spawnResult(2, "", ""), block(shellHookDefaultBlockMessage), ""},
		{pre, spawnResult(2, `{"decision": "block", "reason": "from stdout"}`, "from stderr"), block("from stdout"), ""},
		{spec("on_session_start", false), spawnResult(2, "", "boom"), nil, ""},
		{pre, spawnResult(1, `{"decision": "block", "reason": "nope"}`, ""), block("nope"), ""},
		{pre, spawnResult(1, "", "oops"), nil, ""},
		{pre, errored, nil, ""},
		{preFC, errored, nil, "failed closed: No such file"},
		{pre, timedOut, nil, ""},
		{preFC, timedOut, nil, fmt.Sprintf("timed out after %ds", ShellHookDefaultTimeoutSeconds)},
		{preFC, spawnResult(0, "Traceback (most recent call last): ...", ""), nil, "unparseable stdout"},
		{preFC, spawnResult(1, "", "Traceback: hook crashed"), nil, "hook exited 1 with no directive"},
		{pre, spawnResult(0, "Traceback (most recent call last): ...", ""), nil, ""},
		{preFC, spawnResult(0, "{}", ""), nil, ""},
		{preFC, spawnResult(0, "", ""), nil, ""},
		{spec("on_session_start", true), &ShellHookResult{Error: "boom"}, nil, ""},
	}
	for i, c := range cases {
		got := evaluateShellHookResult(c.spec, c.r)
		if c.has != "" {
			if got == nil || got["action"] != "block" || !strings.Contains(got["message"].(string), c.has) {
				t.Fatalf("case %d = %v, want a block naming %q", i, got, c.has)
			}
		} else if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("case %d = %v, want %v", i, got, c.want)
		}
	}
}

func TestShellHookFailSemanticsEndToEnd(t *testing.T) {
	h := shellHookWorld(t)
	dir, ctx := filepath.Dir(h), context.Background()
	call := map[string]any{"tool_name": "terminal", "args": map[string]any{"command": "rm -rf /"}}
	crash := writeHookScript(t, dir, "crash.sh", "#!/usr/bin/env bash\necho 'RuntimeError: hook crashed' >&2\nexit 1\n")
	got, err := makeShellHookCallback(NewShellHookSpec("pre_tool_call", `bash "`+crash+`"`, "", 60, true))(ctx, call)
	if err != nil || got == nil || !strings.Contains(got.(map[string]any)["message"].(string), "hook exited 1 with no directive") {
		t.Fatalf("crashed fail closed hook %v, %v", got, err)
	}
	exit2 := writeHookScript(t, dir, "exit2.sh", "#!/usr/bin/env bash\necho \"rm -rf is not permitted\" >&2\nexit 2\n")
	got, _ = makeShellHookCallback(NewShellHookSpec("pre_tool_call", exit2, "", 60, false))(ctx, call)
	if !reflect.DeepEqual(got, map[string]any{"action": "block", "message": "rm -rf is not permitted"}) {
		t.Fatalf("exit 2 hook %v", got)
	}
	got, _ = makeShellHookCallback(NewShellHookSpec("pre_tool_call", filepath.Join(dir, "does-not-exist.sh"), "", 60, true))(ctx, call)
	if got == nil || !strings.Contains(got.(map[string]any)["message"].(string), "failed closed") {
		t.Fatalf("missing fail closed hook %v", got)
	}
	denied := writeHookScript(t, dir, "denied.sh", "#!/usr/bin/env bash\necho \"denied\" >&2\nexit 2\n")
	r, err := RunShellHookOnce(ctx, NewShellHookSpec("pre_tool_call", denied, "", 60, false), call)
	if err != nil || *r.ReturnCode != 2 || !reflect.DeepEqual(r.Parsed, map[string]any{"action": "block", "message": "denied"}) {
		t.Fatalf("run once %+v, %v", r, err)
	}
	sleepy := writeHookScript(t, dir, "sleepy.sh", "#!/usr/bin/env bash\nsleep 5\n")
	r, err = RunShellHookOnce(ctx, NewShellHookSpec("pre_tool_call", sleepy, "", 1, true), call)
	if err != nil || !r.TimedOut || r.Parsed["action"] != "block" || !strings.Contains(r.Parsed["message"].(string), "failed closed") {
		t.Fatalf("fail closed timeout %+v, %v", r, err)
	}
}

func TestShellHookChildSeesRoutedProfileHome(t *testing.T) {
	h := shellHookWorld(t)
	routed := filepath.Join(filepath.Dir(h), "routed")
	if err := os.MkdirAll(routed, 0o755); err != nil {
		t.Fatal(err)
	}
	installHostEnvHooks()
	t.Setenv("OPENAI_API_KEY", "sk-default-profile")
	tools.SetMultiplexActive(true)
	t.Cleanup(func() { tools.SetMultiplexActive(false) })
	script := writeHookScript(t, filepath.Dir(h), "env_dump.sh", "#!/usr/bin/env bash\ncat > /dev/null\nprintf '{\"home\": \"%s\", \"key\": \"%s\"}\\n' \"$ICEBATH_HOME\" \"${OPENAI_API_KEY:-}\"\n")
	ctx := home.WithOverride(context.Background(), routed)
	payload, err := serializeShellHookPayload(ctx, "pre_tool_call", map[string]any{"tool_name": "terminal"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := spawnShellHook(ctx, NewShellHookSpec("pre_tool_call", script, "", 60, false), payload)
	if err != nil || r.Error != "" {
		t.Fatalf("spawn %+v, %v", r, err)
	}
	var seen map[string]any
	if err := json.Unmarshal([]byte(r.Stdout), &seen); err != nil || seen["home"] != routed || seen["key"] != "" {
		t.Fatalf("child saw %q (%v)", r.Stdout, err)
	}
	if !strings.Contains(payload, `"profile": `) {
		t.Fatalf("payload lacks the profile %s", payload)
	}
}

func TestShellHookAllowlistReadsUTF8SigAndRejectsBrokenFiles(t *testing.T) {
	shellHookWorld(t)
	ctx := context.Background()
	if data, readable := readShellHookAllowlist(ctx); !readable || len(data["approvals"].([]any)) != 0 {
		t.Fatal("missing allowlist is not an empty readable skeleton")
	}
	p := ShellHookAllowlistPath(ctx)
	for body, want := range map[string]bool{"\ufeff{\"approvals\": [], \"x\": 1}": true, "{corrupt": false, "[]": false, `{"approvals": {}}`: false} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, readable := readShellHookAllowlist(ctx); readable != want {
			t.Fatalf("%q readable = %v", body, readable)
		}
	}
	shellHookAtomicWrite = func(context.Context, string, any, os.FileMode, ...pyjson.Option) error {
		return &os.PathError{Op: "open", Path: p, Err: errors.New("ro")}
	}
	logs := captureLogs(t)
	if err := SaveShellHookAllowlist(ctx, map[string]any{"approvals": []any{}}); err != nil || !strings.Contains(logs.String(), "Failed to persist shell hook allowlist") {
		t.Fatalf("save failure %v %s", err, logs.String())
	}
}

func TestShellHookRegistrationIsPerProfileHome(t *testing.T) {
	h := shellHookWorld(t)
	root := filepath.Dir(h)
	for label, consent := range map[string]bool{"alpha": true, "beta": true, "unapproved": false} {
		dir := filepath.Join(root, label)
		_ = os.MkdirAll(dir, 0o755)
		guard := writeHookScript(t, dir, "guard.sh", fmt.Sprintf("#!/usr/bin/env bash\nprintf '{\"decision\":\"block\",\"reason\":\"%s\"}\\n'\n", label))
		cfg := hooksCfg("pre_tool_call", map[string]any{"command": guard, "matcher": "write_file", "fail_closed": true})
		cfg["hooks_auto_accept"] = consent
		if _, err := RegisterShellHooksFromConfig(home.WithOverride(context.Background(), dir), cfg, false, false); err != nil {
			t.Fatal(err)
		}
	}
	for _, label := range []string{"alpha", "beta", "alpha", "unapproved"} {
		ctx := home.WithOverride(context.Background(), filepath.Join(root, label))
		msg, err := GetPreToolCallBlockMessage(ctx, "write_file", map[string]any{"path": "x"}, PreToolCallIDs{})
		want := label
		if label == "unapproved" {
			want = ""
		}
		if err != nil || msg != want {
			t.Fatalf("%s blocked with %q (%v)", label, msg, err)
		}
	}
}
