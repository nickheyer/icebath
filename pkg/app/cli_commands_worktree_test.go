package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
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
	prev := worktreeChdir
	worktreeChdir = func(string) error { return &os.PathError{Op: "chdir", Path: "/nope", Err: syscall.EACCES} }
	t.Cleanup(func() { worktreeChdir = prev })
	if c.enterNewWorktree(map[string]string{"path": "/nope"}) {
		t.Fatal("a failed chdir entered the tree")
	}
	want := catalogText("cli.commands.worktree.enter_failed", map[string]any{"error": "[Errno 13] Permission denied: '/nope'"})
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
