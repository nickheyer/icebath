package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/tools"
)

// A command handler CLI over a temp home with the worktree state reset and console and cprint captured
func worktreeHandlerCLI(t *testing.T) (*CLI, *bytes.Buffer) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("TERMINAL_CWD", "")
	os.Unsetenv("TERMINAL_CWD")
	activeWorktreeMu.Lock()
	prevActive := activeWorktree
	activeWorktree = nil
	activeWorktreeMu.Unlock()
	atExitMu.Lock()
	prevExit := atExitFuncs
	atExitFuncs = nil
	atExitMu.Unlock()
	var out bytes.Buffer
	restore := SetStreams(&cliui.Console{In: strings.NewReader(""), Out: &out}, &out)
	prevCprint := tools.WorktreeCprint
	tools.WorktreeCprint = func(text string) { out.WriteString(stripANSI(text) + "\n") }
	t.Cleanup(func() {
		restore()
		tools.WorktreeCprint = prevCprint
		activeWorktreeMu.Lock()
		activeWorktree = prevActive
		activeWorktreeMu.Unlock()
		atExitMu.Lock()
		atExitFuncs = prevExit
		atExitMu.Unlock()
	})
	return &CLI{ctx: context.Background()}, &out
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] == ';' || (s[j] >= '0' && s[j] <= '9')) {
				j++
			}
			if j < len(s) && s[j] == 'm' {
				i = j
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// A one commit repository at tmp/proj that the test runs inside
func worktreeRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("git required")
	}
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "HOME=" + repo, "PATH=" + os.Getenv("PATH")}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-m", "init")
	t.Chdir(repo)
	return repo
}

func catalogText(key string, kwargs map[string]any) string {
	return i18n.T(context.Background(), key, "", kwargs)
}

func runWorktree(t *testing.T, c *CLI, cmd string) {
	t.Helper()
	if err := c.handleWorktreeCommand(cmd); err != nil {
		t.Fatal(err)
	}
}

func currentWorktree() map[string]string {
	activeWorktreeMu.Lock()
	defer activeWorktreeMu.Unlock()
	return activeWorktree
}

func TestStatusOutsideRepo(t *testing.T) {
	c, out := worktreeHandlerCLI(t)
	t.Chdir(t.TempDir())
	runWorktree(t, c, "/worktree")
	if !strings.Contains(out.String(), catalogText("cli.commands.worktree.not_in_repo_note", nil)) {
		t.Fatalf("output %q", out.String())
	}
}

func TestNewOutsideRepo(t *testing.T) {
	c, out := worktreeHandlerCLI(t)
	t.Chdir(t.TempDir())
	runWorktree(t, c, "/worktree new")
	if !strings.Contains(out.String(), catalogText("cli.commands.worktree.new_requires_repo", nil)) {
		t.Fatalf("output %q", out.String())
	}
}

func TestListShowsWorktrees(t *testing.T) {
	c, out := worktreeHandlerCLI(t)
	repo := worktreeRepo(t)
	runWorktree(t, c, "/worktree list")
	if !strings.Contains(out.String(), filepath.ToSlash(repo)) {
		t.Fatalf("output %q", out.String())
	}
}

func TestNewNamedCreatesAndRetargets(t *testing.T) {
	c, out := worktreeHandlerCLI(t)
	repo := worktreeRepo(t)
	runWorktree(t, c, "/worktree new fix-login")
	wt := filepath.Join(repo, ".worktrees", "fix-login")
	if !strings.Contains(out.String(), catalogText("cli.commands.worktree.ready", map[string]any{"path": wt})) {
		t.Fatalf("output %q", out.String())
	}
	if info, err := os.Stat(wt); err != nil || !info.IsDir() {
		t.Fatalf("tree %v", err)
	}
	if os.Getenv("TERMINAL_CWD") != wt {
		t.Fatalf("TERMINAL_CWD %q", os.Getenv("TERMINAL_CWD"))
	}
	cwd, _ := os.Getwd()
	realCwd, _ := filepath.EvalSymlinks(cwd)
	realWt, _ := filepath.EvalSymlinks(wt)
	if realCwd != realWt {
		t.Fatalf("cwd %q", cwd)
	}
	active := currentWorktree()
	if active == nil || active["branch"] != "icebath/fix-login" {
		t.Fatalf("active %v", active)
	}
	if body, _ := os.ReadFile(filepath.Join(wt, "a.txt")); string(body) != "hello\n" {
		t.Fatalf("a.txt %q", body)
	}
}

func TestNewNamedCollisionRefused(t *testing.T) {
	c, out := worktreeHandlerCLI(t)
	repo := worktreeRepo(t)
	runWorktree(t, c, "/worktree new dup")
	first := currentWorktree()
	t.Chdir(repo)
	runWorktree(t, c, "/worktree new dup")
	if !strings.Contains(out.String(), "already exists") {
		t.Fatalf("output %q", out.String())
	}
	if again := currentWorktree(); again["path"] != first["path"] || again["branch"] != first["branch"] {
		t.Fatalf("active clobbered %v", again)
	}
}

func TestNewUnnamedUsesRandomHermesPrefix(t *testing.T) {
	c, out := worktreeHandlerCLI(t)
	worktreeRepo(t)
	runWorktree(t, c, "/worktree new")
	active := currentWorktree()
	if !strings.Contains(out.String(), catalogText("cli.commands.worktree.ready", map[string]any{"path": active["path"]})) {
		t.Fatalf("output %q", out.String())
	}
	if name := filepath.Base(active["path"]); !strings.HasPrefix(name, "icebath-") {
		t.Fatalf("name %q", name)
	}
}

func TestNewSanitizesName(t *testing.T) {
	c, _ := worktreeHandlerCLI(t)
	worktreeRepo(t)
	runWorktree(t, c, "/worktree new weird name!@#")
	if name := filepath.Base(currentWorktree()["path"]); name != "weird-name" {
		t.Fatalf("name %q", name)
	}
}

func TestEnterNewWorktreeReportsAFailedMove(t *testing.T) {
	c, out := worktreeHandlerCLI(t)
	missing := filepath.Join(t.TempDir(), "nope")
	if c.enterNewWorktree(map[string]string{"path": missing}) {
		t.Fatal("a failed chdir entered the tree")
	}
	want := catalogText("cli.commands.worktree.enter_failed", map[string]any{"error": "[Errno 2] No such file or directory: '" + missing + "'"})
	if !strings.Contains(out.String(), want) {
		t.Fatalf("output %q want %q", out.String(), want)
	}
	if _, set := os.LookupEnv("TERMINAL_CWD"); set {
		t.Fatal("TERMINAL_CWD moved")
	}
}

func TestWorktreeNewQueuesItsCleanupAndEntersTheTree(t *testing.T) {
	c, _ := worktreeHandlerCLI(t)
	repo := worktreeRepo(t)
	runWorktree(t, c, "/worktree new kept")
	atExitMu.Lock()
	queued := len(atExitFuncs)
	atExitMu.Unlock()
	if queued != 1 {
		t.Fatalf("queued %d exit cleanups", queued)
	}
	t.Chdir(repo)
	runAtExit()
	if _, err := os.Stat(filepath.Join(repo, ".worktrees", "kept")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clean tree kept after exit %v", err)
	}
}

func TestRunAtExitRunsNewestFirstPastAPanic(t *testing.T) {
	worktreeHandlerCLI(t)
	var order []int
	registerAtExit(func() { order = append(order, 1) })
	registerAtExit(func() { panic("boom") })
	registerAtExit(func() { order = append(order, 3) })
	runAtExit()
	if len(order) != 2 || order[0] != 3 || order[1] != 1 {
		t.Fatalf("order %v", order)
	}
	runAtExit()
	if len(order) != 2 {
		t.Fatal("exit functions ran twice")
	}
}

func TestWorktreeStatusUnknownAndPrune(t *testing.T) {
	c, out := worktreeHandlerCLI(t)
	worktreeRepo(t)
	runWorktree(t, c, "/worktree status")
	runWorktree(t, c, "/worktree bogus")
	runWorktree(t, c, "/worktree prune --dry-run")
	for _, want := range []string{
		catalogText("cli.commands.worktree.none_active", nil),
		catalogText("cli.commands.worktree.hint_new", nil),
		catalogText("cli.commands.worktree.unknown_subcommand", map[string]any{"subcommand": "bogus"}),
		catalogText("cli.commands.worktree.usage", nil),
		catalogText("cli.commands.worktree.nothing_to_reclaim", nil),
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// Snapshot the store double answers a resume of a known session with
func storeSnapshot(sessionID string) map[string]any {
	return map[string]any{"session_id": sessionID, "stored_session_id": sessionID, "messages": []any{}, "revision": 3,
		"execution_generation": 0, "subscription_id": "sub-" + sessionID,
		"info": map[string]any{"model": "fake-model", "cwd": "/w", "context_length": 128000, "enabled_toolsets": []any{"file"},
			"tools": map[string]any{"file": []any{"read_file", "write_file"}}, "skills": map[string]any{"devops": []any{"sdlc-review"}},
			"mcp_servers": []any{}, "tool_availability": map[string]any{"available_toolsets": []any{"file"}, "unavailable_toolsets": []any{},
				"lazy_tools": []any{}, "disabled_tools": []any{}}}}
}

// Worktree handler CLI attached to a scripted gateway session, its tools running there
func gatewayWorktreeCLI(t *testing.T, mutate func(client *scriptedClient, params map[string]any) error) (*CLI, *bytes.Buffer, *scriptedClient) {
	t.Helper()
	c, out := worktreeHandlerCLI(t)
	client := &scriptedClient{}
	client.override = func(method string, params map[string]any) (any, bool, error) {
		client.record(method, params)
		switch method {
		case "session.resume":
			if params["session_id"] != "local-s" {
				return nil, true, &gateway.GatewayClientError{Msg: "not_found"}
			}
			return storeSnapshot("local-s"), true, nil
		case "session.mutate":
			if mutate != nil {
				if err := mutate(client, params); err != nil {
					return nil, true, err
				}
			}
		}
		return map[string]any{"status": "ok", "detached": true}, true, nil
	}
	conn := scriptedConnection(t, client)
	c.sessionDB, c.gatewaySessionOpen, c.sessionID = gatewaycli.NewGatewaySessionStore(conn), true, "local-s"
	t.Cleanup(func() { _ = c.sessionDB.CloseReader() })
	return c, out, client
}

func TestNewMovesTheGatewaySessionsWorkspaceIntoTheTree(t *testing.T) {
	c, out, client := gatewayWorktreeCLI(t, nil)
	repo := worktreeRepo(t)
	runWorktree(t, c, "/worktree new moved")
	wt := filepath.Join(repo, ".worktrees", "moved")
	mutations := client.callsOf("session.mutate")
	if len(mutations) != 1 {
		t.Fatalf("mutations %v", mutations)
	}
	params := mutations[0]
	if params["operation"] != "workspace" || !sameJSON(params["payload"], map[string]any{"cwd": wt}) || params["session_id"] != "local-s" {
		t.Fatalf("mutation %v", params)
	}
	if !sameJSON(params["expected_generation"], 0) {
		t.Fatalf("expected generation %v", params["expected_generation"])
	}
	if !strings.Contains(out.String(), catalogText("cli.commands.worktree.tools_in_worktree", nil)) {
		t.Fatalf("output %q", out.String())
	}
}

func TestNewRefusedByTheGatewayNeverClaimsTheToolsMoved(t *testing.T) {
	c, out, _ := gatewayWorktreeCLI(t, func(*scriptedClient, map[string]any) error { return &gateway.GatewayRPCError{Msg: "session_busy"} })
	repo := worktreeRepo(t)
	runWorktree(t, c, "/worktree new busy")
	if !strings.Contains(out.String(), "session_busy") || strings.Contains(out.String(), catalogText("cli.commands.worktree.tools_in_worktree", nil)) {
		t.Fatalf("output %q", out.String())
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	here, _ := filepath.EvalSymlinks(cwd)
	want, _ := filepath.EvalSymlinks(repo)
	if here != want {
		t.Fatalf("cwd %s, want %s", here, want)
	}
	if _, set := os.LookupEnv("TERMINAL_CWD"); set || currentWorktree() != nil {
		t.Fatalf("TERMINAL_CWD set %v active %v", set, currentWorktree())
	}
	if _, err := os.Stat(filepath.Join(repo, ".worktrees", "busy")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused tree stayed on disk: %v", err)
	}
}

func TestNewWhoseShellCannotFollowSaysTheSessionMoved(t *testing.T) {
	var wt string
	c, out, _ := gatewayWorktreeCLI(t, func(_ *scriptedClient, params map[string]any) error {
		payload, _ := params["payload"].(map[string]any)
		wt, _ = payload["cwd"].(string)
		return os.Chmod(wt, 0)
	})
	repo := worktreeRepo(t)
	t.Cleanup(func() {
		if wt != "" {
			_ = os.Chmod(wt, 0o755)
		}
	})
	runWorktree(t, c, "/worktree new stuck")
	if want := filepath.Join(repo, ".worktrees", "stuck"); wt != want {
		t.Fatalf("moved to %q, want %q", wt, want)
	}
	failed := catalogText("cli.session.cwd_enter_failed", map[string]any{"path": wt, "error": "[Errno 13] Permission denied: '" + wt + "'"})
	if !strings.Contains(out.String(), failed) {
		t.Fatalf("output %q lacks %q", out.String(), failed)
	}
	if !strings.Contains(out.String(), catalogText("cli.commands.worktree.tools_in_worktree", nil)) || currentWorktree()["path"] != wt {
		t.Fatalf("output %q active %v", out.String(), currentWorktree())
	}
	if _, set := os.LookupEnv("TERMINAL_CWD"); set {
		t.Fatal("TERMINAL_CWD moved")
	}
}

func TestAMovedSessionGetsItsCleanTreeBackOnResume(t *testing.T) {
	c, _, _ := gatewayWorktreeCLI(t, nil)
	repo := worktreeRepo(t)
	runWorktree(t, c, "/worktree new kept")
	wt := filepath.Join(repo, ".worktrees", "kept")
	t.Chdir(repo)
	cleanupWorktree(c.ctx, currentWorktree())
	if _, err := os.Stat(wt); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the clean tree stayed after quit: %v", err)
	}
	if got := tools.RecreateTUIWorktree(c.ctx, wt, "local-s"); got != wt {
		t.Fatalf("recreated %q", got)
	}
	if info, err := os.Stat(wt); err != nil || !info.IsDir() {
		t.Fatalf("recreated tree %v %v", info, err)
	}
}
