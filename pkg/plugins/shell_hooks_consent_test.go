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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

func shellPidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func readShellMarker(t *testing.T, marker string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(raw)) != "" {
			pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
			return pid
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("hook script never wrote its descendant pid")
	return 0
}

func waitShellPidGone(pid int) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && shellPidAlive(pid) {
		time.Sleep(50 * time.Millisecond)
	}
	if shellPidAlive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		return false
	}
	return true
}

func TestShellHookTreeKill(t *testing.T) {
	h := shellHookWorld(t)
	dir, ctx := filepath.Dir(h), context.Background()
	spec := func(cmd string, timeout int) *ShellHookSpec {
		return NewShellHookSpec("post_tool_call", cmd, "", timeout, false)
	}
	marker := filepath.Join(dir, "child.pid")
	stall := writeHookScript(t, dir, "stall.sh", fmt.Sprintf("#!/usr/bin/env bash\nsleep 300 &\necho $! > %s\nsleep 300\n", marker))
	t0 := time.Now()
	r, err := spawnShellHook(ctx, spec(stall, 1), "{}")
	if err != nil || !r.TimedOut || time.Since(t0) > 30*time.Second {
		t.Fatalf("timeout run %+v, %v", r, err)
	}
	if child := readShellMarker(t, marker); !waitShellPidGone(child) {
		t.Fatalf("descendant %d survived hook timeout", child)
	}
	os.Remove(marker)
	detached := writeHookScript(t, dir, "detached.sh", fmt.Sprintf("#!/usr/bin/env bash\nsleep 300 > /dev/null 2>&1 < /dev/null &\necho $! > %s\nexit 0\n", marker))
	r, err = spawnShellHook(ctx, spec(detached, 10), "{}")
	if err != nil || r.TimedOut || r.ReturnCode == nil || *r.ReturnCode != 0 {
		t.Fatalf("detached run %+v, %v", r, err)
	}
	child := readShellMarker(t, marker)
	time.Sleep(300 * time.Millisecond)
	alive := shellPidAlive(child)
	_ = syscall.Kill(child, syscall.SIGKILL)
	if !alive {
		t.Fatal("successful hook's detached helper must survive")
	}
	pgid := writeHookScript(t, dir, "pgid.sh", "#!/usr/bin/env bash\necho \"$$ $(ps -o pgid= -p $$ | tr -d ' ')\"\n")
	r, _ = spawnShellHook(ctx, spec(pgid, 10), "{}")
	fields := strings.Fields(r.Stdout)
	if len(fields) != 2 || fields[0] != fields[1] || fields[1] == strconv.Itoa(syscall.Getpgrp()) {
		t.Fatalf("hook child does not lead its own group: %q", r.Stdout)
	}
	echoer := writeHookScript(t, dir, "echoer.sh", "#!/usr/bin/env bash\ncat\necho errline >&2\nexit 3\n")
	r, _ = spawnShellHook(ctx, spec(echoer, 10), `{"tool_name": "terminal"}`)
	if *r.ReturnCode != 3 || r.Stdout != `{"tool_name": "terminal"}` || !strings.Contains(r.Stderr, "errline") || r.Error != "" || r.TimedOut {
		t.Fatalf("fast path %+v", r)
	}
	r, _ = spawnShellHook(ctx, spec("/nonexistent/hook-command-xyz", 2), "{}")
	if r.Error != "command not found" || r.ReturnCode != nil {
		t.Fatalf("missing command %+v", r)
	}
	hookPid := filepath.Join(dir, "hook.pid")
	sleeper := writeHookScript(t, dir, "sleeper.sh", fmt.Sprintf("#!/usr/bin/env bash\necho $$ > %q\nsleep 300\n", hookPid))
	cctx, cancel := context.WithCancel(ctx)
	go func() {
		readShellMarker(t, hookPid)
		cancel()
	}()
	_, err = spawnShellHook(cctx, spec(sleeper, 300), "{}")
	var interrupt *UserInterrupt
	if !errors.As(err, &interrupt) {
		t.Fatalf("interrupt did not propagate: %v", err)
	}
	if pid := readShellMarker(t, hookPid); !waitShellPidGone(pid) {
		t.Fatalf("hook %d survived the interrupt", pid)
	}
}

func TestShellHookTTYPromptFlow(t *testing.T) {
	h := shellHookWorld(t)
	ctx := context.Background()
	script := writeHookScript(t, filepath.Dir(h), "hook.sh", "#!/usr/bin/env bash\nprintf '{}\\n'\n")
	cfg := hooksCfg("on_session_start", map[string]any{"command": script})
	shellHookTTY(t, "y")
	registered, err := RegisterShellHooksFromConfig(ctx, cfg, false, true)
	entry := ShellHookAllowlistEntry(ctx, "on_session_start", script)
	if err != nil || len(registered) != 1 || entry == nil || entry["event"] != "on_session_start" || entry["command"] != script {
		t.Fatalf("approved prompt %v %v %v", registered, entry, err)
	}
	if !strings.Contains(shellHookStdout.(*strings.Builder).String(), "Icebath is about to register a shell hook") {
		t.Fatal("prompt text missing")
	}
	ResetShellHooksForTests()
	ResetPluginManagersForTests()
	shellHookTTY(t)
	if registered, _ := RegisterShellHooksFromConfig(ctx, cfg, false, true); len(registered) != 1 {
		t.Fatal("allowlisted hook did not register without a prompt")
	}
	other := writeHookScript(t, filepath.Dir(h), "other.sh", "#!/usr/bin/env bash\nprintf '{}\\n'\n")
	shellHookTTY(t, "n")
	if registered, _ := RegisterShellHooksFromConfig(ctx, hooksCfg("on_session_start", map[string]any{"command": other}), false, true); len(registered) != 0 ||
		ShellHookAllowlistEntry(ctx, "on_session_start", other) != nil {
		t.Fatal("rejected prompt registered or recorded")
	}
}

func TestShellHookNonTTYAndNeverPromptOutsideTheForegroundCLI(t *testing.T) {
	h := shellHookWorld(t)
	ctx := context.Background()
	script := writeHookScript(t, filepath.Dir(h), "hook.sh", "#!/usr/bin/env bash\nprintf '{}\\n'\n")
	cfg := hooksCfg("on_session_start", map[string]any{"command": script})
	if r, _ := RegisterShellHooksFromConfig(ctx, cfg, false, false); len(r) != 0 {
		t.Fatal("registered without a terminal or opt in")
	}
	asked := shellHookTTY(t)
	logs := captureLogs(t)
	if r, _ := RegisterShellHooksFromConfig(ctx, cfg, false, false); len(r) != 0 || *asked != 0 ||
		ShellHookAllowlistEntry(ctx, "on_session_start", script) != nil || !strings.Contains(logs.String(), ShellHookAllowlistPath(ctx)) {
		t.Fatalf("default registration prompted or registered: %s", logs.String())
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("hooks:\n  on_session_start:\n    - command: "+strconvQuote(script)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reRegisterConfigHooks(ctx); err != nil || *asked != 0 || !strings.Contains(logs.String(), "not allowlisted") ||
		ShellHookAllowlistEntry(ctx, "on_session_start", script) != nil {
		t.Fatalf("force reload re-registration prompted: %v", err)
	}
	t.Setenv("ICEBATH_ACCEPT_HOOKS", "1")
	if r, _ := RegisterShellHooksFromConfig(ctx, cfg, false, false); len(r) != 1 || *asked != 0 || ShellHookAllowlistEntry(ctx, "on_session_start", script) == nil {
		t.Fatal("opt in did not approve without a prompt")
	}
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestShellHookAllowlistOps(t *testing.T) {
	h := shellHookWorld(t)
	ctx := context.Background()
	t.Setenv("HOME", filepath.Dir(h))
	writeHookScript(t, filepath.Dir(h), "hook.sh", "#!/usr/bin/env bash\n")
	if err := recordShellHookApproval(ctx, "on_session_start", "~/hook.sh"); err != nil {
		t.Fatal(err)
	}
	if e := ShellHookAllowlistEntry(ctx, "on_session_start", "~/hook.sh"); e == nil || e["script_mtime_at_approval"] == nil {
		t.Fatalf("tilde approval %v", e)
	}
	script := filepath.Join(filepath.Dir(h), "hook.sh")
	_ = recordShellHookApproval(ctx, "on_session_start", script)
	later := time.Now().Add(time.Second)
	_ = os.Chtimes(script, later, later)
	_ = recordShellHookApproval(ctx, "on_session_start", script)
	matching := 0
	for _, e := range LoadShellHookAllowlist(ctx)["approvals"].([]any) {
		if m := e.(map[string]any); m["event"] == "on_session_start" && m["command"] == script {
			matching++
		}
	}
	if matching != 1 {
		t.Fatalf("duplicate approvals %d", matching)
	}
	for v, want := range map[any]bool{true: true, nil: false, int64(1): false, "false": false, " Yes ": true} {
		if got := resolveShellHookAccept(map[string]any{"hooks_auto_accept": v}, false); got != want {
			t.Fatalf("hooks_auto_accept %v = %v", v, got)
		}
	}
	if n, err := RevokeShellHook(ctx, script); err != nil || n != 1 || ShellHookAllowlistEntry(ctx, "on_session_start", script) != nil {
		t.Fatalf("revoke %d %v", n, err)
	}
	if !ShellHookScriptIsExecutable(script) || !ShellHookScriptIsExecutable("bash "+script) || ShellHookScriptIsExecutable(script+".missing") {
		t.Fatal("script executability misjudged")
	}
	_ = os.Chmod(script, 0o644)
	if ShellHookScriptIsExecutable(script) || !ShellHookScriptIsExecutable("bash "+script) {
		t.Fatal("a bare script needs execute and an interpreted one read access")
	}
	if p := shellHookScriptPath("python3 -u ~/hooks/x.py --flag"); p != "~/hooks/x.py" {
		t.Fatalf("script path %q", p)
	}
}

func TestShellHookGatewayResyncFollowsConfigAndAllowlist(t *testing.T) {
	h := shellHookWorld(t)
	dir, ctx := filepath.Dir(h), context.Background()
	shellHookTTY(t)
	a := writeHookScript(t, dir, "a.sh", "#!/bin/sh\nprintf '{}'\n")
	b := writeHookScript(t, dir, "b.sh", "#!/bin/sh\nprintf '{}'\n")
	cfg := hooksCfg("pre_tool_call", map[string]any{"command": a})
	load := func() any { return cfg }
	SyncShellHooksFromConfig(ctx, load)
	if w := wiredShellHooks("pre_tool_call"); len(w) != 0 {
		t.Fatalf("unapproved hook wired %v", w)
	}
	if err := AcceptShellHooksForLaunch(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	SyncShellHooksFromConfig(ctx, load)
	if w := wiredShellHooks("pre_tool_call"); !reflect.DeepEqual(w, []string{"shell_hook[pre_tool_call:" + a + "]"}) {
		t.Fatalf("launch approval wired %v", w)
	}
	hooks := cfg["hooks"].(map[string]any)
	hooks["pre_tool_call"] = append(hooks["pre_tool_call"].([]any), map[string]any{"command": b})
	_ = recordShellHookApproval(ctx, "pre_tool_call", b)
	SyncShellHooksFromConfig(ctx, load)
	SyncShellHooksFromConfig(ctx, load)
	if w := wiredShellHooks("pre_tool_call"); len(w) != 2 {
		t.Fatalf("client approval wired %v", w)
	}
	SyncShellHooksFromConfig(ctx, func() any { return map[string]any{} })
	allowlist := filepath.Join(h, ShellHookAllowlistFilename)
	saved, _ := os.ReadFile(allowlist)
	_ = os.WriteFile(allowlist, []byte("{corrupt"), 0o600)
	SyncShellHooksFromConfig(ctx, load)
	if w := wiredShellHooks("pre_tool_call"); len(w) != 2 {
		t.Fatalf("unreadable input disarmed a guard %v", w)
	}
	_ = os.WriteFile(allowlist, saved, 0o600)
	c := writeHookScript(t, dir, "c.sh", "#!/bin/sh\nprintf '{}'\n")
	hooks["pre_tool_call"] = append(hooks["pre_tool_call"].([]any), map[string]any{"command": c})
	cfg["hooks_auto_accept"] = true
	SyncShellHooksFromConfig(ctx, load)
	if w := wiredShellHooks("pre_tool_call"); len(w) != 3 || ShellHookAllowlistEntry(ctx, "pre_tool_call", c) != nil {
		t.Fatalf("opt in did not keep an unsaved hook %v", w)
	}
	hooks["pre_tool_call"] = hooks["pre_tool_call"].([]any)[:2]
	delete(cfg, "hooks_auto_accept")
	_, _ = RevokeShellHook(ctx, a)
	SyncShellHooksFromConfig(ctx, load)
	if w := wiredShellHooks("pre_tool_call"); !reflect.DeepEqual(w, []string{"shell_hook[pre_tool_call:" + b + "]"}) {
		t.Fatalf("revoked hook still wired %v", w)
	}
	hooks["pre_tool_call"] = []any{}
	SyncShellHooksFromConfig(ctx, load)
	if w := wiredShellHooks("pre_tool_call"); len(w) != 0 {
		t.Fatalf("removed hook still wired %v", w)
	}
}

func TestShellHookResyncKeepsInMemoryConsentAndNeverDisarmsOnBadInput(t *testing.T) {
	h := shellHookWorld(t)
	dir, ctx := filepath.Dir(h), context.Background()
	a := writeHookScript(t, dir, "a.sh", "#!/bin/sh\nprintf '{}'\n")
	b := writeHookScript(t, dir, "b.sh", "#!/bin/sh\nprintf '{}'\n")
	cfg := hooksCfg("pre_tool_call", map[string]any{"command": a})
	load := func() any { return cfg }
	shellHookAtomicWrite = func(context.Context, string, any, os.FileMode, ...pyjson.Option) error {
		return &os.PathError{Op: "open", Path: "x", Err: errors.New("ro")}
	}
	_ = AcceptShellHooksForLaunch(ctx, cfg)
	SyncShellHooksFromConfig(ctx, load)
	SyncShellHooksFromConfig(ctx, load)
	shellHookAtomicWrite = defaultShellHookAtomicWrite
	if w := wiredShellHooks("pre_tool_call"); !reflect.DeepEqual(w, []string{"shell_hook[pre_tool_call:" + a + "]"}) {
		t.Fatalf("unsaved launch consent lost %v", w)
	}
	body, _ := json.Marshal(map[string]any{"approvals": []any{map[string]any{"event": "pre_tool_call", "command": []any{}}, map[string]any{"event": "pre_tool_call", "command": b}}})
	_ = os.WriteFile(filepath.Join(h, ShellHookAllowlistFilename), body, 0o600)
	cfg["hooks"].(map[string]any)["pre_tool_call"] = map[string]any{"command": a}
	SyncShellHooksFromConfig(ctx, load)
	if w := wiredShellHooks("pre_tool_call"); len(w) != 1 {
		t.Fatalf("malformed entry removed a hook %v", w)
	}
	cfg["hooks"].(map[string]any)["pre_tool_call"] = []any{map[string]any{"command": a}, map[string]any{"command": b}}
	SyncShellHooksFromConfig(ctx, load)
	if w := wiredShellHooks("pre_tool_call"); len(w) != 2 {
		t.Fatalf("valid record lost beside a bad one %v", w)
	}
	c := writeHookScript(t, dir, "c.sh", "#!/bin/sh\nprintf '{}'\n")
	cfg["hooks"].(map[string]any)["pre_tool_call"] = append(cfg["hooks"].(map[string]any)["pre_tool_call"].([]any), map[string]any{"command": c})
	SyncShellHooksFromConfig(ctx, load)
	cName := "shell_hook[pre_tool_call:" + c + "]"
	if strings.Contains(strings.Join(wiredShellHooks("pre_tool_call"), ","), cName) {
		t.Fatal("unapproved hook wired")
	}
	AcceptShellHooksForProcess()
	SyncShellHooksFromConfig(ctx, load)
	if !strings.Contains(strings.Join(wiredShellHooks("pre_tool_call"), ","), cName) {
		t.Fatal("process opt in did not wire a later hook")
	}
	ResetShellHooksForTests()
	ResetPluginManagersForTests()
	oldRead, release := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		SyncShellHooksFromConfig(ctx, func() any { close(oldRead); <-release; return hooksCfg("pre_tool_call", map[string]any{"command": a}) })
	}()
	<-oldRead
	go func() {
		defer wg.Done()
		SyncShellHooksFromConfig(ctx, func() any { return hooksCfg("pre_tool_call", map[string]any{"command": b}) })
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if w := wiredShellHooks("pre_tool_call"); !reflect.DeepEqual(w, []string{"shell_hook[pre_tool_call:" + b + "]"}) {
		t.Fatalf("older snapshot landed last %v", w)
	}
}

func TestShellHookLaunchApprovalsAndRefusedEntries(t *testing.T) {
	h := shellHookWorld(t)
	dir, ctx := filepath.Dir(h), context.Background()
	a := writeHookScript(t, dir, "a.sh", "#!/bin/sh\nprintf '{}'\n")
	b := writeHookScript(t, dir, "b.sh", "#!/bin/sh\nprintf '{}'\n")
	cfgA := func() any { return hooksCfg("pre_tool_call", map[string]any{"command": a}) }
	_ = AcceptShellHooksForLaunch(ctx, cfgA())
	_, _ = RevokeShellHook(ctx, a)
	SyncShellHooksFromConfig(ctx, cfgA)
	if w := wiredShellHooks("pre_tool_call"); len(w) != 0 {
		t.Fatalf("revoked saved launch approval wired %v", w)
	}
	body, _ := json.Marshal(map[string]any{"approvals": []any{map[string]any{"event": "pre_tool_call", "command": []any{}}, map[string]any{"event": "pre_tool_call", "command": b}}})
	_ = os.WriteFile(filepath.Join(h, ShellHookAllowlistFilename), body, 0o600)
	SyncShellHooksFromConfig(ctx, func() any { return hooksCfg("pre_tool_call", map[string]any{"command": b}) })
	if w := wiredShellHooks("pre_tool_call"); !reflect.DeepEqual(w, []string{"shell_hook[pre_tool_call:" + b + "]"}) {
		t.Fatalf("bad record broke a valid one %v", w)
	}
	ResetShellHooksForTests()
	ResetPluginManagersForTests()
	_ = os.Remove(filepath.Join(h, ShellHookAllowlistFilename))
	shellHookAtomicWrite = func(context.Context, string, any, os.FileMode, ...pyjson.Option) error {
		return &os.PathError{Op: "open", Path: "x", Err: errors.New("ro")}
	}
	_ = AcceptShellHooksForLaunch(ctx, cfgA())
	shellHookAtomicWrite = defaultShellHookAtomicWrite
	SyncShellHooksFromConfig(ctx, cfgA)
	if w := wiredShellHooks("pre_tool_call"); len(w) != 1 {
		t.Fatalf("unsaved launch consent not wired %v", w)
	}
	SyncShellHooksFromConfig(ctx, func() any { return map[string]any{"hooks": map[string]any{}} })
	SyncShellHooksFromConfig(ctx, cfgA)
	if w := wiredShellHooks("pre_tool_call"); len(w) != 0 {
		t.Fatalf("re-added hook kept ended consent %v", w)
	}
	ResetShellHooksForTests()
	ResetPluginManagersForTests()
	_, _ = RegisterShellHooksFromConfig(ctx, cfgA(), true, false)
	_, _ = RevokeShellHook(ctx, a)
	refused := hooksCfg("pre_tool_call", map[string]any{"command": a})
	refused["hooks"].(map[string]any)["transform_api_error_classification"] = []any{map[string]any{"command": a}}
	SyncShellHooksFromConfig(ctx, func() any { return refused })
	if w := wiredShellHooks("pre_tool_call"); len(w) != 0 {
		t.Fatalf("refused plugin only entry suspended removal %v", w)
	}
	_, _ = RegisterShellHooksFromConfig(ctx, cfgA(), true, false)
	_, _ = RevokeShellHook(ctx, a)
	shellHookTTY(t)
	if r, err := RegisterShellHooksFromConfig(ctx, cfgA(), false, false); err != nil || len(r) != 0 {
		t.Fatalf("re-asked for a wired hook %v %v", r, err)
	}
}

func TestShellHookToolHooksReceiveOnlyTheOwningWorkersIdentity(t *testing.T) {
	h := shellHookWorld(t)
	dir, ctx := filepath.Dir(h), context.Background()
	t.Setenv("ICEBATH_KANBAN_TASK", "t_owner")
	capture := filepath.Join(dir, "captured.json")
	gate := writeHookScript(t, dir, "gate.sh", fmt.Sprintf("#!/usr/bin/env bash\n{ cat; printf '\\n%%s' \"${ICEBATH_KANBAN_TASK-<unset>}\"; } > %q\nprintf '{\"action\":\"block\", \"message\":\"verified gate fired\"}\\n'\n", capture))
	cfg := map[string]any{"hooks": map[string]any{
		"pre_tool_call":  []any{map[string]any{"matcher": "kanban_complete", "command": gate}},
		"post_tool_call": []any{map[string]any{"matcher": "kanban_complete", "command": gate}},
	}}
	if r, err := RegisterShellHooksFromConfig(ctx, cfg, true, false); err != nil || len(r) != 2 {
		t.Fatalf("registered %v %v", r, err)
	}
	nonOwner := &tools.Env{Values: &sync.Map{}}
	nonOwner.Values.Store("session.non_dispatcher_owned", true)
	scopes := []struct {
		ctx  context.Context
		want string
	}{{ctx, "t_owner"}, {tools.WithDelegatedChildContext(ctx, true), ""}, {tools.WithToolEnv(ctx, nonOwner), ""}, {ctx, "t_owner"}}
	read := func() (map[string]any, string) {
		raw, err := os.ReadFile(capture)
		if err != nil {
			t.Fatal(err)
		}
		os.Remove(capture)
		text := string(raw)
		i := strings.LastIndex(text, "\n")
		var payload map[string]any
		if err := json.Unmarshal([]byte(text[:i]), &payload); err != nil {
			t.Fatal(err)
		}
		return payload, text[i+1:]
	}
	for _, s := range scopes {
		msg, err := GetPreToolCallBlockMessage(s.ctx, "kanban_complete", map[string]any{}, PreToolCallIDs{TaskID: "turn-id", SessionID: "session-id", ToolCallID: "call-id"})
		if err != nil || msg != "verified gate fired" {
			t.Fatalf("block %q %v", msg, err)
		}
		payload, workerEnv := read()
		extra := payload["extra"].(map[string]any)
		if extra["kanban_task_id"] != s.want || extra["task_id"] != "turn-id" || payload["session_id"] != "session-id" || workerEnv != "<unset>" {
			t.Fatalf("pre_tool_call payload %v worker env %q, want task %q", payload, workerEnv, s.want)
		}
		GetPluginManager(s.ctx).InvokeHook(s.ctx, "post_tool_call", map[string]any{"tool_name": "kanban_complete", "args": map[string]any{}, "result": "{}", "task_id": "turn-id"})
		payload, _ = read()
		if payload["hook_event_name"] != "post_tool_call" || payload["extra"].(map[string]any)["kanban_task_id"] != s.want {
			t.Fatalf("post_tool_call payload %v", payload)
		}
	}
}
