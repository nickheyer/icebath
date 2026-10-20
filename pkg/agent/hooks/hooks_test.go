package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
)

func cfg(t *testing.T, doc string) Config {
	t.Helper()
	c, err := Load(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var call = protocol.ToolCall{ID: "1", Name: "Bash", Args: json.RawMessage(`{"command":"ls"}`)}

func TestLoadAndValidate(t *testing.T) {
	c := cfg(t, `{"hooks":{"PreToolUse":[{"matcher":"Bash|Edit","hooks":[{"type":"command","command":"true"}]}],"Stop":[{"hooks":[{"command":"true","timeout":5}]}]}}`)
	if len(c[PreToolUse]) != 1 || c[PreToolUse][0].re == nil || c[Stop][0].Hooks[0].Timeout != 5 {
		t.Fatalf("%+v", c)
	}
	if flat := cfg(t, `{"PostToolUse":[{"hooks":[{"command":"true"}]}]}`); len(flat[PostToolUse]) != 1 {
		t.Fatalf("%+v", flat)
	}
	for name, bad := range map[string]string{
		"json":    `{`,
		"event":   `{"Weird":[{"hooks":[{"command":"true"}]}]}`,
		"type":    `{"Stop":[{"hooks":[{"type":"http","command":"x"}]}]}`,
		"empty":   `{"Stop":[{"hooks":[{"command":"  "}]}]}`,
		"matcher": `{"PreToolUse":[{"matcher":"(","hooks":[{"command":"true"}]}]}`,
	} {
		if _, err := Load(strings.NewReader(bad)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	path := filepath.Join(t.TempDir(), "hooks.json")
	os.WriteFile(path, []byte(`{"hooks":{"Stop":[]}}`), 0o644)
	if _, err := LoadFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path + ".nope"); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestPreToolUse(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen.json")
	r := Runner{Config: cfg(t, `{"PreToolUse":[{"matcher":"^Bash$","hooks":[{"command":"cat > `+seen+`"}]},{"matcher":"Edit","hooks":[{"command":"exit 2"}]}]}`), Dir: dir, SessionID: "s-1", Transcript: "/var/sessions/s-1.jsonl"}
	got, settled, err := r.BeforeTool(ctx, call)
	if err != nil || settled != "" || string(got.Args) != string(call.Args) {
		t.Fatalf("%v %+v", err, got)
	}
	data, _ := os.ReadFile(seen)
	var p payload
	json.Unmarshal(data, &p)
	if p.Event != PreToolUse || p.SessionID != "s-1" || p.TranscriptPath != "/var/sessions/s-1.jsonl" || p.ToolName != "Bash" || string(p.ToolInput) != `{"command":"ls"}` || p.Cwd != dir {
		t.Fatalf("%+v", p)
	}
	if _, _, err := r.BeforeTool(ctx, protocol.ToolCall{Name: "Read"}); err != nil {
		t.Fatal("unmatched tools must run no hooks")
	}
	var blocked *Blocked
	if _, _, err := r.BeforeTool(ctx, protocol.ToolCall{Name: "Edit"}); !errors.As(err, &blocked) || blocked.Reason != "blocked by hook" {
		t.Fatalf("%v", err)
	}
	r = Runner{Config: cfg(t, `{"PreToolUse":[{"hooks":[{"command":"echo nope >&2; exit 2"}]}]}`)}
	if _, _, err := r.BeforeTool(ctx, call); !errors.As(err, &blocked) || blocked.Reason != "nope" || !strings.Contains(err.Error(), "PreToolUse blocked: nope") {
		t.Fatalf("%v", err)
	}
	r = Runner{Config: cfg(t, `{"PreToolUse":[{"hooks":[{"command":"cat >/dev/null; echo '{\"decision\":\"block\",\"reason\":\"json says no\"}'"}]}]}`)}
	if _, _, err := r.BeforeTool(ctx, call); !errors.As(err, &blocked) || blocked.Reason != "json says no" {
		t.Fatalf("%v", err)
	}
	r = Runner{Config: cfg(t, `{"PreToolUse":[{"hooks":[{"command":"cat >/dev/null; echo '{\"hookSpecificOutput\":{\"permissionDecision\":\"deny\",\"permissionDecisionReason\":\"policy\"}}'"}]}]}`)}
	if _, _, err := r.BeforeTool(ctx, call); !errors.As(err, &blocked) || blocked.Reason != "policy" {
		t.Fatalf("%v", err)
	}
	r = Runner{Config: cfg(t, `{"PreToolUse":[{"hooks":[{"command":"cat >/dev/null; echo '{\"updatedInput\":{\"command\":\"ls -la\"}}'"},{"command":"cat > `+seen+`; echo '{\"hookSpecificOutput\":{\"updatedInput\":{\"command\":\"ls -lah\"}}}'"}]}]}`)}
	got, _, err = r.BeforeTool(ctx, call)
	if err != nil || string(got.Args) != `{"command":"ls -lah"}` {
		t.Fatalf("%v %s", err, got.Args)
	}
	data, _ = os.ReadFile(seen)
	json.Unmarshal(data, &p)
	if string(p.ToolInput) != `{"command":"ls -la"}` {
		t.Fatalf("later hooks must see rewritten input: %s", p.ToolInput)
	}
	for name, want := range map[string]struct {
		doc      string
		decision policy.Decision
	}{
		"approve":                 {`{"PreToolUse":[{"hooks":[{"command":"cat >/dev/null; echo '{\"decision\":\"approve\"}'"}]}]}`, policy.Allow},
		"allow":                   {`{"PreToolUse":[{"hooks":[{"command":"cat >/dev/null; echo '{\"hookSpecificOutput\":{\"permissionDecision\":\"allow\"}}'"}]}]}`, policy.Allow},
		"ask":                     {`{"PreToolUse":[{"hooks":[{"command":"cat >/dev/null; echo '{\"hookSpecificOutput\":{\"permissionDecision\":\"ask\"}}'"}]}]}`, policy.Ask},
		"ask beats allow":         {`{"PreToolUse":[{"hooks":[{"command":"cat >/dev/null; echo '{\"hookSpecificOutput\":{\"permissionDecision\":\"ask\"}}'"},{"command":"cat >/dev/null; echo '{\"decision\":\"approve\"}'"}]}]}`, policy.Ask},
		"silence settles nothing": {`{"PreToolUse":[{"hooks":[{"command":"cat >/dev/null"}]}]}`, ""},
	} {
		r = Runner{Config: cfg(t, want.doc)}
		if _, settled, err := r.BeforeTool(ctx, call); err != nil || settled != want.decision {
			t.Fatalf("%s: %v %q", name, err, settled)
		}
	}
	for name, doc := range map[string]string{
		"bad input":  `{"PreToolUse":[{"hooks":[{"command":"cat >/dev/null; echo '{\"updatedInput\":[1]}'"}]}]}`,
		"other exit": `{"PreToolUse":[{"hooks":[{"command":"echo broken >&2; exit 1"}]}]}`,
		"timeout":    `{"PreToolUse":[{"hooks":[{"command":"sleep 5","timeout":1}]}]}`,
		"decision":   `{"PreToolUse":[{"hooks":[{"command":"cat >/dev/null; echo '{\"decision\":\"maybe\"}'"}]}]}`,
		"permission": `{"PreToolUse":[{"hooks":[{"command":"cat >/dev/null; echo '{\"hookSpecificOutput\":{\"permissionDecision\":\"later\"}}'"}]}]}`,
	} {
		r = Runner{Config: cfg(t, doc)}
		_, _, err := r.BeforeTool(ctx, call)
		if err == nil || errors.As(err, &blocked) {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "other exit" && !strings.Contains(err.Error(), "broken") {
			t.Fatalf("%v", err)
		}
	}
	var log bytes.Buffer
	r = Runner{Config: cfg(t, `{"PreToolUse":[{"hooks":[{"command":"cat >/dev/null; echo plain words; echo warn >&2"}]}]}`), Log: &log}
	if _, _, err := r.BeforeTool(ctx, call); err != nil {
		t.Fatal(err)
	}
	if log.String() != "[hook PreToolUse] warn\n[hook PreToolUse] plain words\n" {
		t.Fatalf("%q", log.String())
	}
	raw := Runner{Config: Config{PreToolUse: []Group{{Matcher: "B.*", Hooks: []Command{{Command: "exit 2"}}}}}}
	if _, _, err := raw.BeforeTool(ctx, call); !errors.As(err, &blocked) {
		t.Fatalf("uncompiled matchers must still match: %v", err)
	}
	raw = Runner{Config: Config{PreToolUse: []Group{{Matcher: "(", Hooks: []Command{{Command: "exit 2"}}}}}}
	if _, _, err := raw.BeforeTool(ctx, call); err == nil || errors.As(err, &blocked) {
		t.Fatalf("%v", err)
	}
}

func TestOtherEvents(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen.json")
	r := Runner{Config: cfg(t, `{"UserPromptSubmit":[{"hooks":[{"command":"cat > `+seen+`; echo '{\"additionalContext\":\"from hook\"}'"}]}],"PostToolUse":[{"matcher":"Bash","hooks":[{"command":"cat >> `+seen+`; echo '{\"additionalContext\":\"checked\"}'"}]}],"Stop":[{"hooks":[{"command":"cat > `+seen+`; echo '{\"decision\":\"block\",\"reason\":\"keep going\"}'"}]}]}`), Dir: dir}
	input, err := r.BeforeRun(ctx, []protocol.Part{protocol.Text{Text: "hi "}, protocol.Image{MediaType: "image/png"}, protocol.Text{Text: "there"}})
	if err != nil || len(input) != 4 || input[3].(protocol.Text).Text != "from hook" {
		t.Fatalf("%v %+v", err, input)
	}
	data, _ := os.ReadFile(seen)
	var p payload
	json.Unmarshal(data, &p)
	if p.Event != UserPromptSubmit || p.Prompt != "hi there" {
		t.Fatalf("%+v", p)
	}
	if err := r.BeforeTurn(ctx, &protocol.Request{}); err != nil {
		t.Fatal(err)
	}
	os.Remove(seen)
	res, err := r.AfterTool(ctx, call, protocol.ToolResult{CallID: "1", Name: "Bash", Content: []protocol.Part{protocol.Text{Text: "out"}, protocol.Image{MediaType: "image/png", Data: []byte{1}}}, IsError: true})
	if err != nil || len(res.Content) != 3 || res.Content[2].(protocol.Text).Text != "checked" || !res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	data, _ = os.ReadFile(seen)
	json.Unmarshal(data, &p)
	if p.Event != PostToolUse || string(p.ToolResponse) != `{"content":[{"type":"text","text":"out"},{"type":"image","media_type":"image/png","data":"AQ=="}],"is_error":true}` || p.ToolName != "Bash" {
		t.Fatalf("%+v %s", p, p.ToolResponse)
	}
	misplaced := Runner{Config: cfg(t, `{"PostToolUse":[{"hooks":[{"command":"cat >/dev/null; echo '{\"hookSpecificOutput\":{\"permissionDecision\":\"allow\"}}'"}]}],"Stop":[{"hooks":[{"command":"cat >/dev/null; echo '{\"updatedInput\":{\"a\":1}}'"}]}]}`)}
	if _, err := misplaced.AfterTool(ctx, call, protocol.ToolResult{}); err == nil || !strings.Contains(err.Error(), "only PreToolUse") {
		t.Fatalf("%v", err)
	}
	if _, err := misplaced.OnStop(ctx, protocol.Response{}); err == nil || !strings.Contains(err.Error(), "only PreToolUse") {
		t.Fatalf("%v", err)
	}
	if _, err := r.AfterTool(ctx, protocol.ToolCall{Name: "Read"}, protocol.ToolResult{}); err != nil {
		t.Fatal(err)
	}
	cont, err := r.OnStop(ctx, protocol.Response{Content: []protocol.Part{protocol.Text{Text: "final"}}})
	if err != nil || len(cont) != 1 || cont[0].(protocol.Text).Text != "keep going" {
		t.Fatalf("%v %+v", err, cont)
	}
	data, _ = os.ReadFile(seen)
	json.Unmarshal(data, &p)
	if p.Event != Stop || p.Response != "final" {
		t.Fatalf("%+v", p)
	}
	quiet := Runner{Config: cfg(t, `{"Stop":[{"hooks":[{"command":"cat >/dev/null"}]}]}`)}
	if cont, err := quiet.OnStop(ctx, protocol.Response{}); err != nil || cont != nil {
		t.Fatalf("%v %+v", err, cont)
	}
	blocking := Runner{Config: cfg(t, `{"UserPromptSubmit":[{"hooks":[{"command":"echo denied >&2; exit 2"}]}],"PostToolUse":[{"hooks":[{"command":"exit 2"}]}]}`)}
	var blocked *Blocked
	if _, err := blocking.BeforeRun(ctx, nil); !errors.As(err, &blocked) || blocked.Reason != "denied" {
		t.Fatalf("%v", err)
	}
	if _, err := blocking.AfterTool(ctx, call, protocol.ToolResult{}); !errors.As(err, &blocked) {
		t.Fatalf("%v", err)
	}
	env := Runner{Config: cfg(t, `{"Stop":[{"hooks":[{"command":"test \"$ICEBATH_SESSION_ID\" = s-9 && test \"$ICEBATH_HOOK_EVENT\" = Stop && test \"$EXTRA\" = yes || exit 1"}]}]}`), SessionID: "s-9", Env: []string{"EXTRA=yes"}}
	if _, err := env.OnStop(ctx, protocol.Response{}); err != nil {
		t.Fatal(err)
	}
}
