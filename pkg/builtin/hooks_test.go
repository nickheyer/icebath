package builtin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const fakeLauncher = `#!/bin/sh
printf '%s' "$0" > "$FAKE_RECORD/launcher.txt"
cat > "$FAKE_RECORD/stdin.json"
cat "$FAKE_RECORD/stdin.json" >> "$FAKE_RECORD/events.txt"
echo >> "$FAKE_RECORD/events.txt"
env > "$FAKE_RECORD/env.txt"
printf '%s' "$*" > "$FAKE_RECORD/argv.txt"
cat "$FAKE_RECORD/stdout.txt" 2>/dev/null
exit 0
`

const postFindings = `{"hookSpecificOutput": {"hookEventName": "PostToolUse", "additionalContext": "Gradient text in App.tsx"}}`

// Fixture for the design hook tests, a trusted project with a launcher and a record directory
type hookFixture struct {
	t                          *testing.T
	tmp, project, record, home string
	trusted                    map[string]bool
	env                        map[string]string
	materialized               string
}

func newHookFixture(t *testing.T) *hookFixture {
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	f := &hookFixture{t: t, tmp: tmp, project: filepath.Join(tmp, "project"), record: filepath.Join(tmp, "record"), home: filepath.Join(tmp, "empty-home")}
	for _, d := range []string{filepath.Join(f.project, "src"), f.record, f.home} {
		os.MkdirAll(d, 0o755)
	}
	installFakeLauncher(t, filepath.Join(f.project, ".icebath"))
	f.trusted = map[string]bool{f.project: true}
	f.env = map[string]string{}
	t.Setenv("FAKE_RECORD", f.record)
	return f
}

func installFakeLauncher(t *testing.T, root string) {
	scripts := filepath.Join(root, "skills", "impeccable", "scripts")
	os.MkdirAll(scripts, 0o755)
	if err := os.WriteFile(filepath.Join(scripts, "impeccable"), []byte(fakeLauncher), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (f *hookFixture) hook(cwd string) *designHook {
	h := newDesignHook(func(context.Context) string { return cwd })
	h.homeDir = func(context.Context) string { return f.home }
	h.trusted = func(_ context.Context, root string) bool { return f.trusted[root] }
	h.materialized = func() string { return f.materialized }
	h.getenv = func(k string) string { return f.env[k] }
	return h
}

func (f *hookFixture) reset(stdout string) {
	for _, n := range []string{"stdin.json", "events.txt", "env.txt", "argv.txt", "launcher.txt"} {
		os.Remove(filepath.Join(f.record, n))
	}
	os.WriteFile(filepath.Join(f.record, "stdout.txt"), []byte(stdout), 0o644)
}

func (f *hookFixture) spawned() bool { return isFile(filepath.Join(f.record, "stdin.json")) }

func (f *hookFixture) read(name string) string {
	data, _ := os.ReadFile(filepath.Join(f.record, name))
	return string(data)
}

func (f *hookFixture) event() map[string]any {
	var ev map[string]any
	if err := json.Unmarshal([]byte(f.read("stdin.json")), &ev); err != nil {
		f.t.Fatal(err)
	}
	return ev
}

func transform(t *testing.T, h *designHook, kw map[string]any) any {
	out, err := h.transformToolResult(context.Background(), kw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func preVerify(t *testing.T, h *designHook, kw map[string]any) any {
	out, err := h.preVerify(context.Background(), kw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func jsonEqual(t *testing.T, got any, want string) {
	var a, b any
	json.Unmarshal([]byte(got.(string)), &a)
	json.Unmarshal([]byte(want), &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("got %v want %s", got, want)
	}
}

func TestDesignHookEditCases(t *testing.T) {
	f := newHookFixture(t)
	h := f.hook(f.project)
	f.reset(postFindings)
	if out := transform(t, h, map[string]any{"tool_name": "read_file", "args": map[string]any{"path": "src/App.tsx"}, "result": "{}"}); out != nil || f.spawned() {
		t.Fatal("ignores tools other than write_file and patch")
	}
	if out := transform(t, h, map[string]any{"tool_name": "write_file", "args": map[string]any{"path": "src/App.tsx"}, "result": `{"error": "denied"}`}); out != nil || f.spawned() {
		t.Fatal("leaves an error result untouched")
	}
	out := transform(t, h, map[string]any{"tool_name": "write_file", "args": map[string]any{"path": "src/App.tsx"}, "result": `{"bytes_written": 12}`, "session_id": "s1"})
	jsonEqual(t, out, `{"bytes_written": 12, "impeccable": "Gradient text in App.tsx"}`)
	want := map[string]any{"hook_event_name": "PostToolUse", "cwd": f.project, "tool_name": "Write", "tool_input": map[string]any{"file_path": filepath.Join(f.project, "src/App.tsx")}, "session_id": "s1"}
	if !reflect.DeepEqual(f.event(), want) || f.read("argv.txt") != "hook" || !strings.Contains("\n"+f.read("env.txt"), "\nIMPECCABLE_HOOK_HARNESS=hermes\n") {
		t.Fatalf("write event %v argv %q", f.event(), f.read("argv.txt"))
	}
	f.reset(postFindings)
	resolved := filepath.Join(f.project, "src/styles.css")
	result, _ := json.Marshal(map[string]any{"success": true, "files_modified": []string{resolved}})
	out = transform(t, h, map[string]any{"tool_name": "patch", "args": map[string]any{"path": "elsewhere.css"}, "result": string(result)})
	jsonEqual(t, out, `{"success": true, "files_modified": ["`+resolved+`"], "impeccable": "Gradient text in App.tsx"}`)
	if ev := f.event(); ev["tool_name"] != "Edit" || ev["tool_input"].(map[string]any)["file_path"] != resolved || ev["session_id"] != nil {
		t.Fatalf("patch event %v", ev)
	}
	f.reset(postFindings)
	files := []string{filepath.Join(f.project, "src/a.css"), filepath.Join(f.project, "src/b.css")}
	result, _ = json.Marshal(map[string]any{"success": true, "files_modified": files})
	out = transform(t, h, map[string]any{"tool_name": "patch", "args": map[string]any{"patch": "*** Begin Patch"}, "result": string(result)})
	var parsed map[string]any
	json.Unmarshal([]byte(out.(string)), &parsed)
	lines := strings.Split(strings.TrimSpace(f.read("events.txt")), "\n")
	if parsed["impeccable"] != "Gradient text in App.tsx\n\nGradient text in App.tsx" || len(lines) != 2 || !strings.Contains(lines[0], "a.css") || !strings.Contains(lines[1], "b.css") {
		t.Fatalf("multi file patch %v %v", parsed, lines)
	}
	f.reset("")
	if out := transform(t, h, map[string]any{"tool_name": "write_file", "args": map[string]any{"path": "src/App.tsx"}, "result": "{}"}); out != nil || !f.spawned() {
		t.Fatal("returns None when the engine has nothing to say")
	}
	f.reset(postFindings)
	if out := transform(t, h, map[string]any{"tool_name": "write_file", "args": map[string]any{"path": "src/App.tsx"}, "result": "Wrote src/App.tsx"}); out != "Wrote src/App.tsx\n\nGradient text in App.tsx" {
		t.Fatalf("non JSON result %v", out)
	}
	if got := withFindings(`{"a": "é"}`, "impeccable", "x"); got != `{"a": "é", "impeccable": "x"}` {
		t.Fatalf("ensure_ascii false dump %s", got)
	}
}

func TestDesignHookStopCases(t *testing.T) {
	f := newHookFixture(t)
	h := f.hook(f.project)
	f.reset(`{"decision": "block", "reason": "Deep pass: 2 findings"}`)
	kw := map[string]any{"session_id": "s2", "attempt": 0, "changed_paths": []any{"src/App.tsx"}}
	if out := preVerify(t, h, kw); !reflect.DeepEqual(out, map[string]any{"decision": "block", "reason": "Deep pass: 2 findings"}) {
		t.Fatalf("block %v", out)
	}
	if !reflect.DeepEqual(f.event(), map[string]any{"hook_event_name": "Stop", "cwd": f.project, "stop_hook_active": false, "session_id": "s2"}) {
		t.Fatalf("stop event %v", f.event())
	}
	kw["attempt"] = 1
	preVerify(t, h, kw)
	if f.event()["stop_hook_active"] != false {
		t.Fatal("every nudge scans again; the engine dedupes")
	}
	f.reset(`{"hookSpecificOutput": {"hookEventName": "Stop", "additionalContext": "Deep pass findings"}}`)
	if out := preVerify(t, h, map[string]any{"session_id": "s3"}); !reflect.DeepEqual(out, map[string]any{"action": "continue", "message": "Deep pass findings"}) {
		t.Fatalf("older engine %v", out)
	}
	f.reset("")
	if out := preVerify(t, h, map[string]any{"session_id": "s4"}); out != nil {
		t.Fatalf("empty output %v", out)
	}
}

func TestDesignHookLauncherDiscovery(t *testing.T) {
	f := newHookFixture(t)
	args := map[string]any{"tool_name": "write_file", "args": map[string]any{"path": "src/App.tsx"}, "result": "{}"}
	f.reset(postFindings)
	f.env["IMPECCABLE_HOOK_DISABLED"] = "1"
	if transform(t, f.hook(f.project), args) != nil || preVerify(t, f.hook(f.project), map[string]any{"session_id": "s5"}) != nil || f.spawned() {
		t.Fatal("spawns nothing when IMPECCABLE_HOOK_DISABLED is set")
	}
	delete(f.env, "IMPECCABLE_HOOK_DISABLED")
	outside := filepath.Join(f.tmp, "outside")
	os.MkdirAll(outside, 0o755)
	outsideArgs := map[string]any{"tool_name": "write_file", "args": map[string]any{"path": "a.tsx"}, "result": "{}"}
	if transform(t, f.hook(outside), outsideArgs) != nil || preVerify(t, f.hook(outside), map[string]any{"session_id": "s6"}) != nil || f.spawned() {
		t.Fatal("is a no-op where no launcher is reachable")
	}
	homeDir := filepath.Join(f.tmp, "icebath-home")
	installFakeLauncher(t, homeDir)
	f.home = homeDir
	if out := transform(t, f.hook(outside), outsideArgs); out != `{"impeccable": "Gradient text in App.tsx"}` || f.event()["tool_input"].(map[string]any)["file_path"] != filepath.Join(outside, "a.tsx") {
		t.Fatalf("falls back to the launcher in ICEBATH_HOME %v", out)
	}
	f.reset(postFindings)
	transform(t, f.hook(f.project), args)
	if f.read("launcher.txt") != filepath.Join(f.project, ".icebath/skills/impeccable/scripts/impeccable") {
		t.Fatalf("trusted project launcher %s", f.read("launcher.txt"))
	}
	f.trusted = map[string]bool{}
	transform(t, f.hook(f.project), args)
	if f.read("launcher.txt") != filepath.Join(homeDir, "skills/impeccable/scripts/impeccable") {
		t.Fatalf("untrusted falls back to the home launcher %s", f.read("launcher.txt"))
	}
	f.home = filepath.Join(f.tmp, "empty-home")
	f.reset(postFindings)
	if transform(t, f.hook(f.project), args) != nil || f.spawned() {
		t.Fatal("untrusted, no home launcher")
	}
	materialized := filepath.Join(f.tmp, "materialized")
	installFakeLauncher(t, materialized)
	f.materialized = filepath.Join(materialized, "skills/impeccable/scripts/impeccable")
	if got := f.hook(f.project).findLauncher(context.Background(), f.project); got != f.materialized {
		t.Fatalf("materialized tree launcher %s", got)
	}
}

func TestDesignHookStopReusesSessionLauncher(t *testing.T) {
	f := newHookFixture(t)
	h := f.hook(f.tmp)
	f.reset(`{"decision": "block", "reason": "Deep pass: 1 finding"}`)
	transform(t, h, map[string]any{"tool_name": "write_file", "args": map[string]any{"path": filepath.Join(f.project, "src/App.tsx")}, "result": "{}", "session_id": "s7"})
	out := preVerify(t, h, map[string]any{"session_id": "s7", "attempt": 0, "changed_paths": []any{}})
	if !reflect.DeepEqual(out, map[string]any{"decision": "block", "reason": "Deep pass: 1 finding"}) ||
		f.read("launcher.txt") != filepath.Join(f.project, ".icebath/skills/impeccable/scripts/impeccable") || f.event()["cwd"] != f.tmp {
		t.Fatalf("session launcher %v %s", out, f.read("launcher.txt"))
	}
}

func TestHallmarkLintHookAddsFloorFailures(t *testing.T) {
	f := newHookFixture(t)
	script := filepath.Join(f.tmp, "lint-hook")
	os.WriteFile(script, []byte(fakeLauncher), 0o755)
	var asked string
	installed := false
	h := &hallmarkHook{sessionCwd: func(context.Context) string { return f.project },
		installed: func(_ context.Context, cwd string) bool { return installed && cwd == f.project },
		command: func(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
			asked = name
			return exec.CommandContext(ctx, script), nil
		}}
	f.reset(`{"hookSpecificOutput": {"hookEventName": "PostToolUse", "additionalContext": "Hallmark Floor check on a.html: 1 FAIL."}}`)
	if out, _ := h.transformToolResult(context.Background(), map[string]any{"tool_name": "write_file", "args": map[string]any{"path": "a.html"}, "result": "{}"}); out != nil || f.spawned() {
		t.Fatalf("lint hook ran before install-hook %v", out)
	}
	installed = true
	out, err := h.transformToolResult(context.Background(), map[string]any{"tool_name": "write_file", "args": map[string]any{"path": "a.html"}, "result": `{"bytes_written": 3}`, "session_id": "s"})
	if err != nil || asked != "lint-hook" {
		t.Fatal(err, asked)
	}
	jsonEqual(t, out, `{"bytes_written": 3, "hallmark": "Hallmark Floor check on a.html: 1 FAIL."}`)
	if ev := f.event(); ev["tool_input"].(map[string]any)["file_path"] != filepath.Join(f.project, "a.html") || ev["tool_name"] != "Write" {
		t.Fatalf("lint event %v", ev)
	}
	f.reset("{}")
	if out, _ := h.transformToolResult(context.Background(), map[string]any{"tool_name": "patch", "args": map[string]any{"path": "a.css"}, "result": "{}"}); out != nil {
		t.Fatalf("clean lint %v", out)
	}
	if obj, _ := pyjson.Loads(`{"hookSpecificOutput": {"additionalContext": 3}}`); additionalContext(obj.(*pyjson.Object)) != "" {
		t.Fatal("non string additionalContext")
	}
}
