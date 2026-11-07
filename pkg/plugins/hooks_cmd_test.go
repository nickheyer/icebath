package plugins

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Isolated home for the hooks command with a config document and an outbound target double
func hooksCLIHome(t *testing.T, cfgYAML string) (string, context.Context) {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_ACCEPT_HOOKS", "")
	os.Unsetenv("ICEBATH_ACCEPT_HOOKS")
	ResetShellHooksForTests()
	t.Cleanup(ResetShellHooksForTests)
	if cfgYAML != "" {
		writeFile(t, filepath.Join(h, "config.yaml"), cfgYAML)
	}
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	prev := installedServices()
	var s Services
	if prev != nil {
		s = *prev
	}
	s.OutboundWebhookTargets = func(map[string]any) []OutboundWebhookTarget { return nil }
	InstallServices(&s)
	t.Cleanup(func() { InstallServices(prev) })
	return tmp, homeCtx(h)
}

// Executable hook script under dir
func hookScript(t *testing.T, dir, body, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// Captured output of one hooks command invocation
func runHooks(t *testing.T, ctx context.Context, args HooksArgs) string {
	t.Helper()
	var buf bytes.Buffer
	if err := HooksCommand(ctx, args, &buf); err != nil {
		t.Fatalf("hooks %s: %v", args.Action, err)
	}
	return buf.String()
}

func TestShowsConfiguredAndConsentStatus(t *testing.T) {
	tmp, _ := hooksCLIHome(t, "")
	script := hookScript(t, tmp, "#!/usr/bin/env bash\nprintf '{}\\n'\n", "hook.sh")
	_, ctx := hooksCLIHome(t, "hooks:\n  pre_tool_call:\n    - matcher: terminal\n      command: "+script+"\n      timeout: 30\n"+
		"  on_session_start:\n    - command: "+script+"\n")
	if err := recordShellHookApproval(ctx, "pre_tool_call", script); err != nil {
		t.Fatal(err)
	}
	out := runHooks(t, ctx, HooksArgs{Action: "list"})
	for _, want := range []string{"[pre_tool_call]", "[on_session_start]", "✓ allowed", "✗ not allowlisted", script} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output lacks %q:\n%s", want, out)
		}
	}
}

func TestSyntheticPayloadMatchesProductionShape(t *testing.T) {
	tmp, _ := hooksCLIHome(t, "")
	capture := filepath.Join(tmp, "captured.json")
	script := hookScript(t, tmp, "#!/usr/bin/env bash\ncat - > "+capture+"\nprintf '{}\\n'\n", "hook.sh")
	_, ctx := hooksCLIHome(t, "hooks:\n  subagent_stop:\n    - command: "+script+"\n")
	runHooks(t, ctx, HooksArgs{Action: "test", Event: "subagent_stop"})
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := pyjson.Loads(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	seen := doc.(*pyjson.Object)
	keys := map[string]bool{}
	for _, k := range seen.Keys {
		keys[k] = true
	}
	want := []string{"hook_event_name", "tool_name", "tool_input", "session_id", "cwd", "extra", "profile"}
	if len(keys) != len(want) {
		t.Fatalf("payload keys %v, want %v", seen.Keys, want)
	}
	for _, k := range want {
		if !keys[k] {
			t.Fatalf("payload keys %v lack %s", seen.Keys, k)
		}
	}
	if seen.Vals["session_id"] != "parent-sess" {
		t.Fatalf("session_id = %v", seen.Vals["session_id"])
	}
	extra := seen.Vals["extra"].(*pyjson.Object)
	if _, ok := extra.Vals["parent_session_id"]; ok {
		t.Fatal("parent_session_id stayed in extra")
	}
	if seen.Vals["tool_name"] != nil || seen.Vals["tool_input"] != nil {
		t.Fatalf("tool fields = %v %v, want null", seen.Vals["tool_name"], seen.Vals["tool_input"])
	}
}

func TestFiresRealSubprocessAndParsesBlock(t *testing.T) {
	tmp, _ := hooksCLIHome(t, "")
	block := hookScript(t, tmp, "#!/usr/bin/env bash\nprintf '{\"decision\": \"block\", \"reason\": \"nope\"}\\n'\n", "block.sh")
	_, ctx := hooksCLIHome(t, "hooks:\n  pre_tool_call:\n    - matcher: terminal\n      command: "+block+"\n")
	out := runHooks(t, ctx, HooksArgs{Action: "test", Event: "pre_tool_call", ForTool: "terminal"})
	if !strings.Contains(out, `"action": "block"`) || !strings.Contains(out, `"message": "nope"`) {
		t.Fatalf("parsed block missing:\n%s", out)
	}
}

func TestRevokeRemovesEntry(t *testing.T) {
	tmp, ctx := hooksCLIHome(t, "")
	script := hookScript(t, tmp, "#!/usr/bin/env bash\n", "hook.sh")
	if err := recordShellHookApproval(ctx, "on_session_start", script); err != nil {
		t.Fatal(err)
	}
	out := runHooks(t, ctx, HooksArgs{Action: "revoke", Command: script})
	if !strings.Contains(out, "Removed 1") {
		t.Fatalf("revoke output:\n%s", out)
	}
	if ShellHookAllowlistEntry(ctx, "on_session_start", script) != nil {
		t.Fatal("allowlist entry survived revoke")
	}
}

func TestFlagsMtimeDrift(t *testing.T) {
	tmp, _ := hooksCLIHome(t, "")
	script := hookScript(t, tmp, "#!/usr/bin/env bash\nprintf '{}\\n'\n", "hook.sh")
	_, ctx := hooksCLIHome(t, "hooks:\n  on_session_start:\n    - command: "+script+"\n")
	path := ShellHookAllowlistPath(ctx)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	doc, err := pyjson.Dumps(map[string]any{"approvals": []any{map[string]any{"event": "on_session_start", "command": script,
		"approved_at": "2000-01-01T00:00:00Z", "script_mtime_at_approval": "2000-01-01T00:00:00Z"}}})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, doc)
	out := runHooks(t, ctx, HooksArgs{Action: "doctor"})
	if !strings.Contains(out, "modified since approval") {
		t.Fatalf("doctor output lacks the drift warning:\n%s", out)
	}
}

func TestUnallowlistedScriptIsNotExecuted(t *testing.T) {
	tmp, _ := hooksCLIHome(t, "")
	sentinel := filepath.Join(tmp, "executed")
	script := hookScript(t, tmp, "#!/usr/bin/env bash\ntouch "+sentinel+"\nprintf '{}\\n'\n", "hook.sh")
	_, ctx := hooksCLIHome(t, "hooks:\n  on_session_start:\n    - command: "+script+"\n")
	out := runHooks(t, ctx, HooksArgs{Action: "doctor"})
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("doctor executed an un-allowlisted script — M4 gate regressed")
	}
	if !strings.Contains(strings.ToLower(out), "not allowlisted") || !strings.Contains(out, "skipped JSON smoke test") {
		t.Fatalf("doctor output:\n%s", out)
	}
}

func TestPrintRunResultShowsDecisionForErrorAndTimeout(t *testing.T) {
	parsed := map[string]any{"action": "block", "message": "hook failed closed: command not found"}
	var buf bytes.Buffer
	if err := printHookRunResult(&buf, &ShellHookResult{Error: "command not found", Parsed: parsed}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"✗ error: command not found", `"action": "block"`, `"message": "hook failed closed: command not found"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("error result lacks %q:\n%s", want, out)
		}
	}
	buf.Reset()
	if err := printHookRunResult(&buf, &ShellHookResult{TimedOut: true, ElapsedSeconds: 2.33, Parsed: parsed}); err != nil {
		t.Fatal(err)
	}
	out = buf.String()
	if !strings.Contains(out, "✗ timed out after 2.33s") || !strings.Contains(out, `"action": "block"`) {
		t.Fatalf("timeout result:\n%s", out)
	}
}

func TestHooksTestDistinguishesFailClosedFromFailOpen(t *testing.T) {
	_, ctx := hooksCLIHome(t, "hooks:\n  pre_tool_call:\n    - matcher: terminal\n      command: /nonexistent/hook-closed.sh\n      fail_closed: true\n"+
		"    - matcher: terminal\n      command: /nonexistent/hook-open.sh\nhooks_auto_accept: true\n")
	out := runHooks(t, ctx, HooksArgs{Action: "test", Event: "pre_tool_call", ForTool: "terminal"})
	closed, open, ok := strings.Cut(out, "/nonexistent/hook-open.sh")
	if !ok {
		t.Fatalf("open hook missing:\n%s", out)
	}
	if !strings.Contains(closed, "✗ error:") || !strings.Contains(open, "✗ error:") {
		t.Fatalf("both hooks must report the error:\n%s", out)
	}
	if !strings.Contains(closed, `"action": "block"`) || !strings.Contains(closed, "failed closed") {
		t.Fatalf("fail closed hook lacks its block:\n%s", closed)
	}
	if strings.Contains(open, `"action": "block"`) || !strings.Contains(open, "contributed nothing") {
		t.Fatalf("fail open hook rendered a block:\n%s", open)
	}
}

func TestHooksCommandUsageUnknownAndOutboundTargets(t *testing.T) {
	_, ctx := hooksCLIHome(t, "")
	if out := runHooks(t, ctx, HooksArgs{}); !strings.HasPrefix(out, "Usage: icebath hooks {list|test|revoke|doctor}\n") {
		t.Fatalf("usage output:\n%s", out)
	}
	if out := runHooks(t, ctx, HooksArgs{Action: "bogus"}); out != "Unknown hooks subcommand: bogus\n" {
		t.Fatalf("unknown output %q", out)
	}
	if out := runHooks(t, ctx, HooksArgs{Action: "ls"}); !strings.HasPrefix(out, "No shell hooks or outbound webhooks configured") {
		t.Fatalf("empty list output:\n%s", out)
	}
	s := *installedServices()
	s.OutboundWebhookTargets = func(map[string]any) []OutboundWebhookTarget {
		return []OutboundWebhookTarget{{Label: "audit", URL: "https://example.test/hook", Events: []string{"pre_tool_call", "post_tool_call"},
			Matcher: "terminal", Timeout: 10}}
	}
	InstallServices(&s)
	out := runHooks(t, ctx, HooksArgs{Action: "list"})
	want := "No shell hooks configured in ~/.icebath/config.yaml.\nConfigured outbound webhooks (1 total):\n\n  - audit\n" +
		"      url:     https://example.test/hook\n      events:  pre_tool_call, post_tool_call matcher='terminal' (timeout=10s, UNSIGNED)\n\n"
	if out != want {
		t.Fatalf("outbound list:\n%s\nwant:\n%s", out, want)
	}
	if out := runHooks(t, ctx, HooksArgs{Action: "test", Event: "nope"}); !strings.HasPrefix(out, "Unknown event: 'nope'\nValid events: ") {
		t.Fatalf("unknown event output:\n%s", out)
	}
}
