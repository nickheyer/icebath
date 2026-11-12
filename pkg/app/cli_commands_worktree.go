package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	atExitMu    sync.Mutex
	atExitFuncs []func()
)

// Queues a function for process exit, run after everything else in reverse registration order
func registerAtExit(fn func()) {
	atExitMu.Lock()
	atExitFuncs = append(atExitFuncs, fn)
	atExitMu.Unlock()
}

// Runs and clears the queued exit functions newest first, a panicking one not stopping the rest
func runAtExit() {
	atExitMu.Lock()
	funcs := atExitFuncs
	atExitFuncs = nil
	atExitMu.Unlock()
	for i := len(funcs) - 1; i >= 0; i-- {
		func() {
			defer func() { _ = recover() }()
			funcs[i]()
		}()
	}
}

// Catalog text of the classic CLI command handlers, the cli.commands keys
func (c *CLI) commandsT(key string, kwargs map[string]any) string {
	return c.t("cli.commands."+key, kwargs)
}

// Prints each line on the console
func printLines(lines ...string) {
	out := console().Out
	for _, line := range lines {
		fmt.Fprintln(out, line)
	}
}

// Subcommands of /worktree and the handler each one runs, every handler needing a repo root
var worktreeSubcommands = map[string]string{
	"prune": "prune", "gc": "prune", "clean": "prune",
	"list": "list", "ls": "list",
	"new": "new", "add": "new", "create": "new",
}

// Handles /worktree with status, new, list and prune of isolated git worktrees
func (c *CLI) handleWorktreeCommand(cmdOriginal string) error {
	parts := pystr.SplitWhitespace(cmdOriginal, 2)
	sub, rest := "", ""
	if len(parts) > 1 {
		sub = pystr.Lower(parts[1])
	}
	if len(parts) > 2 {
		rest = pystr.Strip(parts[2])
	}
	repoRoot := tools.GitRepoRoot()
	if sub == "" || sub == "status" || sub == "show" {
		activeWorktreeMu.Lock()
		active := activeWorktree
		activeWorktreeMu.Unlock()
		if active != nil {
			printLines("  "+c.commandsT("worktree.active", map[string]any{"path": active["path"]}),
				"  "+c.commandsT("worktree.branch", map[string]any{"branch": active["branch"]}))
		} else {
			printLines("  " + c.commandsT("worktree.none_active", nil))
		}
		if repoRoot != "" {
			printLines("  "+c.commandsT("worktree.hint_new", nil), "  "+c.commandsT("worktree.hint_prune", nil))
		} else {
			printLines("  " + c.commandsT("worktree.not_in_repo_note", nil))
		}
		return nil
	}
	handler, ok := worktreeSubcommands[sub]
	if !ok {
		printLines("  "+c.commandsT("worktree.unknown_subcommand", map[string]any{"subcommand": sub}), "  "+c.commandsT("worktree.usage", nil))
		return nil
	}
	if repoRoot == "" {
		if handler == "new" {
			printLines("  " + c.commandsT("worktree.new_requires_repo", nil))
		} else {
			printLines("  " + c.commandsT("worktree.not_in_repo", nil))
		}
		return nil
	}
	switch handler {
	case "prune":
		return c.worktreePrune(repoRoot, rest)
	case "list":
		c.worktreeList(repoRoot)
	default:
		c.worktreeNew(repoRoot, rest)
	}
	return nil
}

// Prints git worktree list for the repo, the list failed line when git fails
func (c *CLI) worktreeList(repoRoot string) {
	out := ""
	res, err := pysubprocess.RunWith(c.ctxOrBackground(), []string{"git", "worktree", "list"}, pysubprocess.Options{Dir: repoRoot, Timeout: 10 * time.Second})
	if err == nil && res.Returncode == 0 {
		text, _ := pycodecs.DecodeReplace(res.Stdout, "utf-8")
		out = pystr.Strip(text)
	}
	if out == "" {
		printLines("  " + c.commandsT("worktree.list_failed", nil))
		return
	}
	for _, line := range pystr.Splitlines(out, false) {
		printLines("  " + line)
	}
}

// Context of the CLI, the background context before one is bound
func (c *CLI) ctxOrBackground() context.Context {
	if c.ctx != nil {
		return c.ctx
	}
	return context.Background()
}

// Creates a worktree and moves this session into it, the tree cleaned on exit unless it holds work
func (c *CLI) worktreeNew(repoRoot, rest string) {
	ctx := c.ctxOrBackground()
	syncBase := true
	if cfg, err := config.Load(home.Dir(ctx), config.NewSecrets(home.Dir(ctx)).Lookup()); err == nil {
		if v, ok := cfg["worktree_sync"]; ok {
			syncBase = pyrepr.Truthy(v)
		}
	}
	wtInfo := tools.SetupWorktree(ctx, repoRoot, syncBase, rest, nil)
	if len(wtInfo) == 0 {
		return
	}
	registerAtExit(func() { cleanupWorktree(ctx, wtInfo) })
	if !c.enterNewWorktree(wtInfo) {
		return
	}
	activeWorktreeMu.Lock()
	activeWorktree = wtInfo
	activeWorktreeMu.Unlock()
	printLines("  "+c.commandsT("worktree.ready", map[string]any{"path": wtInfo["path"]}),
		"  "+c.commandsT("worktree.branch", map[string]any{"branch": wtInfo["branch"]}),
		"  "+c.commandsT("worktree.tools_in_worktree", nil))
}

// Reclaims stale worktrees and merged branches, never the tree this session sits in
func (c *CLI) worktreePrune(repoRoot, rest string) error {
	ctx := c.ctxOrBackground()
	rest = pystr.Lower(rest)
	dryRun := strings.Contains(rest, "--dry-run") || containsWord(pystr.SplitWhitespace(rest, -1), "-n")
	activeWorktreeMu.Lock()
	active := activeWorktree
	activeWorktreeMu.Unlock()
	records, err := tools.AuditWorktrees(ctx, repoRoot, false, nil)
	if err != nil {
		return err
	}
	kept := []tools.TreeRecord{}
	for _, r := range records {
		if active == nil || r.Path != active["path"] {
			kept = append(kept, r)
		}
	}
	actions, err := tools.ReclaimWorktrees(ctx, repoRoot, dryRun, kept)
	if err != nil {
		return err
	}
	branchActions, err := tools.ReclaimBranches(repoRoot, dryRun, nil)
	if err != nil {
		return err
	}
	actions = append(actions, branchActions...)
	if len(actions) > 0 {
		key := "worktree.actions_done"
		if dryRun {
			key = "worktree.actions_planned"
		}
		for _, line := range actions {
			printLines("  " + line)
		}
		printLines("  " + c.commandsT(key, map[string]any{"count": len(actions)}))
	} else {
		printLines("  " + c.commandsT("worktree.nothing_to_reclaim", nil))
	}
	var preserved []tools.TreeRecord
	for _, r := range kept {
		if r.Verdict == "keep" && !strings.Contains(r.Reason, "kanban") && !strings.Contains(r.Reason, "in use") {
			preserved = append(preserved, r)
		}
	}
	if len(preserved) > 0 {
		printLines("  " + c.commandsT("worktree.preserved", map[string]any{"count": len(preserved)}))
		for _, r := range preserved {
			printLines("    " + r.Name + ": " + r.Reason)
		}
	}
	return nil
}

func containsWord(words []string, w string) bool {
	for _, x := range words {
		if x == w {
			return true
		}
	}
	return false
}

// Moves the session into the fresh tree through the gateway first, true when its tools now run there
func (c *CLI) enterNewWorktree(wtInfo map[string]string) bool {
	err := gatewaycli.MoveCLIWorkspace(c.ctx, gatewaycli.WorkspaceCLI{SessionDB: c.sessionDB, GatewaySessionOpen: c.gatewaySessionOpen,
		SessionID: c.sessionID}, wtInfo)
	switch {
	case err == nil:
	case isGatewayClientError(err):
		printLines("  " + c.commandsT("worktree.enter_failed", map[string]any{"error": refusalText(c.ctx, err)}))
		activeWorktreeMu.Lock()
		active := activeWorktree
		activeWorktreeMu.Unlock()
		cleanupWorktree(c.ctx, wtInfo)
		activeWorktreeMu.Lock()
		activeWorktree = active
		activeWorktreeMu.Unlock()
		return false
	case gatewayConnectionFor(c) == nil:
		printLines("  " + c.commandsT("worktree.enter_failed", map[string]any{"error": pypath.OSErrorText(err)}))
		return false
	default:
		printLines("  " + c.t("cli.session.cwd_enter_failed", map[string]any{"path": wtInfo["path"], "error": pypath.OSErrorText(err)}))
	}
	return true
}
