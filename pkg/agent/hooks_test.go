package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
)

func TestMatcherForms(t *testing.T) {
	cases := []struct {
		matcher string
		subject string
		want    bool
	}{
		{"", "Bash", true},
		{"*", "Edit", true},
		{"Edit|Write", "Write", true},
		{"Edit|Write", "NotebookEdit", false},
		{"Edit, Write", "Edit", true},
		{"Edit.*", "NotebookEdit", true},
		{"^Edit$", "NotebookEdit", false},
		{"mcp__memory", "mcp__memory__store", false},
		{"mcp__memory__.*", "mcp__memory__store", true},
	}
	for _, c := range cases {
		g := HookGroup{Matcher: c.matcher}
		if err := g.compile(PreToolUse); err != nil {
			t.Fatal(err)
		}
		if got := g.matches(c.subject); got != c.want {
			t.Errorf("%q on %q = %v, want %v", c.matcher, c.subject, got, c.want)
		}
	}
	fc := HookGroup{Matcher: ".envrc|.env"}
	fc.compile(FileChanged)
	if !fc.matches(".env") {
		t.Fatal("FileChanged matchers list literal names split on bars")
	}
	if got := (HookConfig{FileChanged: []HookGroup{fc}}).Watched(); len(got) != 2 {
		t.Fatalf("watched %v", got)
	}
}

func TestParseHooksWrapper(t *testing.T) {
	if _, err := ParseHooks([]byte(`{"PreToolUse":[{"hooks":[{"type":"command","command":"x"}]}]}`), true); err == nil {
		t.Fatal("a plugin file needs the hooks wrapper")
	}
	cfg, err := ParseHooks([]byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"x","if":"Bash(git *)","once":true}]}]}}`), true)
	if err != nil || len(cfg[PreToolUse]) != 1 {
		t.Fatalf("%v %+v", err, cfg)
	}
	if _, err := ParseHooks([]byte(`{"hooks":{"Nope":[]}}`), false); err == nil {
		t.Fatal("unknown events fail")
	}
	if _, err := ParseHooks([]byte(`{"hooks":{"Stop":[{"hooks":[{"type":"prompt"}]}]}}`), false); err == nil {
		t.Fatal("a prompt hook needs a prompt")
	}
	if _, err := ParseHooks([]byte(`{"hooks":{"modules":["./x.js"]}}`), false); err == nil {
		t.Fatal("empty modules pass, present ones are refused")
	}
	if _, err := ParseHooks([]byte(`{"modules":["./x.js"],"hooks":{}}`), false); err == nil {
		t.Fatal("hook modules need a JavaScript host")
	}
}

func TestMergeHooksDedupsSettings(t *testing.T) {
	a := HookConfig{Stop: []HookGroup{{Hooks: []HookCommand{{Type: "command", Command: "echo"}}}}}.Labeled(SourceSettings, "", nil)
	b := HookConfig{Stop: []HookGroup{{Hooks: []HookCommand{{Type: "command", Command: "echo"}}}}}.Labeled(SourceSettings, "", nil)
	c := HookConfig{Stop: []HookGroup{{Hooks: []HookCommand{{Type: "command", Command: "echo"}}}}}.Labeled(SourcePlugin, "p", nil)
	merged := MergeHooks(a, b, c)
	n := 0
	for _, g := range merged[Stop] {
		n += len(g.Hooks)
	}
	if n != 2 {
		t.Fatalf("settings copies run once and plugin copies stay, got %d", n)
	}
}

func TestParseOutput(t *testing.T) {
	r := HookRunner{}
	v, parsed, err := r.parse(PreToolUse, HookCommand{}, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"no"}}`)
	if err != nil || !parsed || !v.Block || v.Reason != "no" || v.Decision != policy.Deny {
		t.Fatalf("deny %+v %v", v, err)
	}
	v, parsed, err = r.parse(PreToolUse, HookCommand{}, `{"decision":"approve","hookSpecificOutput":{"hookEventName":"PreToolUse","updatedInput":{"command":"ls"}}}`)
	if err != nil || !parsed || v.Decision != policy.Allow || string(v.Input) != `{"command":"ls"}` {
		t.Fatalf("approve %+v %v", v, err)
	}
	v, parsed, _ = r.parse(Stop, HookCommand{}, `{"decision":"block","reason":"keep going","continue":true}`)
	if !parsed || !v.Block || v.Reason != "keep going" {
		t.Fatalf("stop block %+v", v)
	}
	v, parsed, _ = r.parse(UserPromptSubmit, HookCommand{}, `{"continue":false,"stopReason":"halt","systemMessage":"hi"}`)
	if !parsed || !v.Stop || v.StopReason != "halt" || v.System != "hi" {
		t.Fatalf("continue false %+v", v)
	}
	v, parsed, _ = r.parse(PostToolUse, HookCommand{}, `{"hookSpecificOutput":{"hookEventName":"PostToolUse","updatedToolOutput":"replaced","additionalContext":"ctx"}}`)
	if !parsed || v.Response != "replaced" || v.Context != "ctx" {
		t.Fatalf("post tool %+v", v)
	}
	if _, parsed, _ := r.parse(Stop, HookCommand{}, "plain words"); parsed {
		t.Fatal("plain text is not JSON")
	}
	if _, _, err := r.parse(Stop, HookCommand{}, `{"decision":"maybe"}`); err == nil {
		t.Fatal("unknown decisions fail")
	}
	if _, parsed, _ := r.parse(StopFailure, HookCommand{}, `{"decision":"block"}`); !parsed {
		t.Fatal("discarded events still parse")
	}
	v, _, _ = r.parse(PermissionRequest, HookCommand{}, `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow","updatedPermissions":[{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"git *"}],"behavior":"allow","destination":"session"}]}}}`)
	if v.Behavior != "allow" || len(v.Updates) != 1 || v.Updates[0].Rules[0].RuleContent != "git *" {
		t.Fatalf("permission request %+v", v)
	}
}

func TestCommandHooksExitCodes(t *testing.T) {
	cfg, err := ParseHooks([]byte(`{"hooks":{
		"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo nope >&2; exit 2"}]}],
		"PostToolUse":[{"hooks":[{"type":"command","command":"echo '{\"hookSpecificOutput\":{\"hookEventName\":\"PostToolUse\",\"additionalContext\":\"seen\"}}'"}]}],
		"UserPromptSubmit":[{"hooks":[{"type":"command","command":"cat >/dev/null; echo more context"}]}],
		"Stop":[{"hooks":[{"type":"command","command":"exit 3"}]}]
	}}`), true)
	if err != nil {
		t.Fatal(err)
	}
	r := HookRunner{Config: cfg, State: &HookState{}}
	ctx := context.Background()
	call := protocol.ToolCall{ID: "1", Name: "Bash", Args: json.RawMessage(`{"command":"ls"}`)}
	if _, err := r.BeforeTool(ctx, call); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("exit 2 blocks the tool with stderr: %v", err)
	}
	if adv, err := r.BeforeTool(ctx, protocol.ToolCall{ID: "2", Name: "Read", Args: json.RawMessage(`{}`)}); err != nil || adv.Decision != "" {
		t.Fatalf("unmatched tools pass: %+v %v", adv, err)
	}
	res, err := r.AfterTool(ctx, call, protocol.ToolResult{CallID: "1", Name: "Bash", Content: []protocol.Part{protocol.Text{Text: "out"}}})
	if err != nil || len(res.Content) != 2 || !strings.Contains(protocolText(res.Content), "seen") {
		t.Fatalf("post tool context %+v %v", res, err)
	}
	input, err := r.BeforeRun(ctx, []protocol.Part{protocol.Text{Text: "hi"}})
	if err != nil || len(input) != 2 || !strings.Contains(protocolText(input), "more context") {
		t.Fatalf("plain stdout joins the prompt: %+v %v", input, err)
	}
	if parts, err := r.OnStop(ctx, protocol.Response{}, false); err != nil || parts != nil {
		t.Fatalf("a failing stop hook does not continue the run: %v %v", parts, err)
	}
}

func TestStopBlockCap(t *testing.T) {
	cfg, _ := ParseHooks([]byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo '{\"decision\":\"block\",\"reason\":\"again\"}'"}]}]}}`), true)
	r := HookRunner{Config: cfg, State: &HookState{}}
	ctx := context.Background()
	continued := 0
	for i := 0; i < stopBlockCap+2; i++ {
		parts, err := r.OnStop(ctx, protocol.Response{}, i > 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(parts) > 0 {
			continued++
		}
	}
	if continued != stopBlockCap {
		t.Fatalf("stop hooks continue %d times, got %d", stopBlockCap, continued)
	}
}

func TestPromptHook(t *testing.T) {
	cfg, _ := ParseHooks([]byte(`{"hooks":{"Stop":[{"hooks":[{"type":"prompt","prompt":"judge $ARGUMENTS"}]}]}}`), true)
	seen := ""
	r := HookRunner{Config: cfg, State: &HookState{}, Host: HookHost{Prompt: func(ctx context.Context, model, prompt string) (string, error) {
		seen = prompt
		return `{"ok": false, "reason": "not done"}`, nil
	}}}
	parts, err := r.OnStop(context.Background(), protocol.Response{Content: []protocol.Part{protocol.Text{Text: "done"}}}, false)
	if err != nil || len(parts) == 0 || !strings.Contains(protocolText(parts), "not done") {
		t.Fatalf("prompt hook block %v %v", parts, err)
	}
	if !strings.Contains(seen, `"hook_event_name":"Stop"`) || strings.Contains(seen, "$ARGUMENTS") {
		t.Fatalf("prompt %q", seen)
	}
}

func TestSubstituteVars(t *testing.T) {
	got := substitute("run ${CLAUDE_PLUGIN_ROOT}/x and $CLAUDE_PLUGIN_ROOT", map[string]string{"CLAUDE_PLUGIN_ROOT": "/p"})
	if got != "run /p/x and /p" {
		t.Fatalf("substitute %q", got)
	}
}

func TestValidSequence(t *testing.T) {
	if !validSequence("\x1b]0;title\x07") || !validSequence("\x1b]9;note\x1b\\") {
		t.Fatal("title and notification sequences pass")
	}
	if validSequence("\x1b]52;c;data\x07") || validSequence("plain") {
		t.Fatal("clipboard and plain text are refused")
	}
}
