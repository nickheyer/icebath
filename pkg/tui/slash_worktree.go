package tui

import (
	"context"
	"os"
	"strings"
	"sync"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Trees /worktree new created in this process, the newest the active one, each settled on quit
var newWorktrees struct {
	sync.Mutex
	active  *Worktree
	created []Worktree
}

// Subcommands of /worktree and the handler each runs
var worktreeHandlers = map[string]string{"prune": "prune", "gc": "prune", "clean": "prune", "list": "list", "ls": "list",
	"new": "new", "add": "new", "create": "new"}

// Tree /worktree new made active, nil when none is
func activeNewWorktree() *Worktree {
	newWorktrees.Lock()
	defer newWorktrees.Unlock()
	return newWorktrees.active
}

// Reference the gateway takes for a tree
func worktreeRef(tree Worktree) tuigateway.WorktreeRef {
	return tuigateway.WorktreeRef{Path: tree.Path, Branch: tree.Branch, RepoRoot: tree.RepoRoot}
}

// Lines of the status form, the active tree or none, with the hints when inside a repository
func worktreeStatusLines(ctx context.Context, active *Worktree, inRepo bool) []string {
	var lines []string
	if active != nil {
		lines = append(lines, commandText(ctx, "worktree.active", map[string]any{"path": active.Path}),
			commandText(ctx, "worktree.branch", map[string]any{"branch": active.Branch}))
	} else {
		lines = append(lines, commandText(ctx, "worktree.none_active", nil))
	}
	if inRepo {
		return append(lines, commandText(ctx, "worktree.hint_new", nil), commandText(ctx, "worktree.hint_prune", nil))
	}
	return append(lines, commandText(ctx, "worktree.not_in_repo_note", nil))
}

// Lines of a prune answer, the reclaimed or planned actions with their count and the trees kept for a reason
func worktreePruneLines(ctx context.Context, res *tuigateway.WorktreePruneResult, dryRun bool) []string {
	if res.Error != nil {
		return []string{"error: " + *res.Error}
	}
	lines := append([]string{}, res.Actions...)
	if len(lines) > 0 {
		key := "worktree.actions_done"
		if dryRun {
			key = "worktree.actions_planned"
		}
		lines = append(lines, commandText(ctx, key, map[string]any{"count": len(lines)}))
	} else {
		lines = append(lines, commandText(ctx, "worktree.nothing_to_reclaim", nil))
	}
	if len(res.Preserved) > 0 {
		lines = append(lines, commandText(ctx, "worktree.preserved", map[string]any{"count": len(res.Preserved)}))
		for _, kept := range res.Preserved {
			lines = append(lines, "  "+kept.Name+": "+kept.Reason)
		}
	}
	return lines
}

// Asks the gateway for the folder's repository and the tree a -w launch of this process locked
func (a *app) worktreeStatus(cwd string, then func(repoRoot string, launch *Worktree, err error) tea.Cmd) tea.Cmd {
	params := tuigateway.WorktreeStatusParams{Cwd: cwd, LaunchCwd: os.Getenv("ICEBATH_CWD"), PID: int64(os.Getpid())}
	return call(a.gw, tuigateway.WorktreeStatus, params, func(res *tuigateway.WorktreeStatusResult, err error) tea.Cmd {
		if err != nil {
			return then("", nil, err)
		}
		root := val(res.RepoRoot)
		var launch *Worktree
		if res.Launch != nil && res.Launch.Path != "" {
			launch = &Worktree{Path: res.Launch.Path, Branch: res.Launch.Branch}
		}
		return then(root, launch, nil)
	})
}

// Tree the session works in, a /worktree new tree first, then the tree of a -w launch
func (a *app) withActiveWorktree(cwd string, then func(active *Worktree, repoRoot string, err error) tea.Cmd) tea.Cmd {
	return a.worktreeStatus(cwd, func(root string, launch *Worktree, err error) tea.Cmd {
		if active := activeNewWorktree(); active != nil {
			return then(active, root, err)
		}
		return then(launch, root, err)
	})
}

// Creates a tree through the gateway and moves the session into it, the creation report first
func (a *app) worktreeNew(r *slashRun, cwd, name string) tea.Cmd {
	params := tuigateway.WorktreeCreateParams{Cwd: cwd, Name: name, PID: int64(os.Getpid())}
	return call(a.gw, tuigateway.WorktreeCreate, params, func(res *tuigateway.WorktreeCreateResult, err error) tea.Cmd {
		if err != nil {
			r.sys("error: " + rpcErrorMessage(err))
			return nil
		}
		if res.RepoRoot == nil {
			r.sys(commandText(a.ctx, "worktree.new_requires_repo", nil))
			return nil
		}
		if len(res.Lines) > 0 {
			r.sys(strings.Join(res.Lines, "\n"))
		}
		if res.Tree == nil || res.Tree.Path == "" {
			return nil
		}
		return a.enterWorktree(r, Worktree{Path: res.Tree.Path, Branch: res.Tree.Branch, RepoRoot: res.Tree.RepoRoot})
	})
}

// Moves the session's workspace into the tree through the gateway, then this process and later sessions follow
func (a *app) enterWorktree(r *slashRun, tree Worktree) tea.Cmd {
	ctx := a.ctx
	ref := worktreeRef(tree)
	entered := func() tea.Cmd {
		newWorktrees.Lock()
		active := tree
		newWorktrees.active = &active
		newWorktrees.created = append(newWorktrees.created, tree)
		newWorktrees.Unlock()
		lines := []string{commandText(ctx, "worktree.ready", map[string]any{"path": tree.Path}),
			commandText(ctx, "worktree.branch", map[string]any{"branch": tree.Branch}), commandText(ctx, "worktree.tools_in_worktree", nil)}
		if err := a.followWorktree(tree.Path); err != nil {
			lines = append(lines, i18n.T(ctx, "cli.session.cwd_enter_failed", "", map[string]any{"path": tree.Path, "error": err.Error()}))
		}
		r.sys(strings.Join(lines, "\n"))
		return call(a.gw, tuigateway.WorktreeRecord, ref, func(_ *tuigateway.WorktreeRecordResult, err error) tea.Cmd {
			if err != nil {
				r.sys("error: " + rpcErrorMessage(err))
			}
			return nil
		})
	}
	refused := func(err error) tea.Cmd {
		r.sys(commandText(ctx, "worktree.enter_failed", map[string]any{"error": rpcErrorMessage(err)}))
		return a.settleRefusedWorktree(r, ref)
	}
	if r.sid == "" {
		return entered()
	}
	if a.gw.canonical() {
		return a.store.mutateCanonical(a.gw, r.sid, "workspace", map[string]any{"cwd": tree.Path}, r.stale, func(m *mutationOutcome, err error) tea.Cmd {
			switch {
			case err != nil:
				return refused(err)
			case m == nil:
				return a.settleRefusedWorktree(r, ref)
			}
			return entered()
		})
	}
	key := a.store.ui.StoredSid
	if key == "" {
		key = r.sid
	}
	return call(a.gw, tuigateway.SessionWorkspaceMove, tuigateway.SessionWorkspaceMoveParams{SessionKey: key, Cwd: tree.Path}, func(_ *tuigateway.SessionWorkspaceMoveResult, err error) tea.Cmd {
		if err != nil {
			return refused(err)
		}
		return entered()
	})
}

// Removes a tree the session could not move into, or shows why the gateway kept it
func (a *app) settleRefusedWorktree(r *slashRun, ref tuigateway.WorktreeRef) tea.Cmd {
	return call(a.gw, tuigateway.WorktreeSettle, ref, func(res *tuigateway.WorktreeSettleResult, err error) tea.Cmd {
		switch {
		case err != nil:
			r.sys("error: " + rpcErrorMessage(err))
		case len(res.Lines) > 0:
			r.sys(strings.Join(res.Lines, "\n"))
		}
		return nil
	})
}

// Points this process, its terminal cwd and the sessions it creates next at the tree
func (a *app) followWorktree(path string) error {
	if info := a.store.ui.Info; info != nil {
		info.Cwd = &path
	}
	if err := os.Chdir(path); err != nil {
		return err
	}
	keys := []string{"TERMINAL_CWD", "ICEBATH_CWD"}
	if os.Getenv("ICEBATH_TUI_CWD") != "" {
		keys = append(keys, "ICEBATH_TUI_CWD")
	}
	for _, key := range keys {
		if err := os.Setenv(key, path); err != nil {
			return err
		}
	}
	a.gw.locked(func() {
		launch := a.gw.env
		a.gw.env = func(key string) string {
			if key == "ICEBATH_CWD" {
				return path
			}
			return launch(key)
		}
	})
	return nil
}

// Hands the trees /worktree new made to the launcher once the terminal UI is gone
func (a *app) settleNewWorktrees() {
	newWorktrees.Lock()
	created := append([]Worktree(nil), newWorktrees.created...)
	active := ""
	if newWorktrees.active != nil {
		active = newWorktrees.active.Path
	}
	newWorktrees.Unlock()
	if len(created) > 0 {
		a.host.SettleWorktrees(a.ctx, created, active)
	}
}

// Subcommand and the rest of a /worktree argument line
func worktreeArgs(arg string) (string, string) {
	arg = strings.TrimSpace(arg)
	at := strings.IndexFunc(arg, unicode.IsSpace)
	if at < 0 {
		return strings.ToLower(arg), ""
	}
	return strings.ToLower(arg[:at]), strings.TrimSpace(arg[at:])
}

// Whether a prune argument asks for a dry run
func worktreeDryRun(rest string) bool {
	rest = strings.ToLower(rest)
	if strings.Contains(rest, "--dry-run") {
		return true
	}
	for _, word := range strings.Fields(rest) {
		if word == "-n" {
			return true
		}
	}
	return false
}

// Slash command of isolated git worktrees, status, list, new and prune
func (a *app) worktreeCommands() []slashCommand {
	return []slashCommand{
		{name: "worktree", help: "show, list, create, or prune isolated git worktrees", usage: "[new [name]|list|prune [--dry-run]]",
			suggest: func(string) []string { return []string{"new", "list", "prune"} },
			run: func(r *slashRun) tea.Cmd {
				sub, rest := worktreeArgs(r.arg)
				ctx, cwd := a.ctx, a.store.sessionCwd()
				handler, known := worktreeHandlers[sub]
				if sub == "" || sub == "status" || sub == "show" {
					handler, known = "status", true
				}
				if !known {
					r.sys(commandText(ctx, "worktree.unknown_subcommand", map[string]any{"subcommand": sub}) + "\n" + commandText(ctx, "worktree.usage", nil))
					return r.ran()
				}
				failed := func(err error) tea.Cmd {
					r.sys("error: " + rpcErrorMessage(err))
					return nil
				}
				switch handler {
				case "status":
					return tea.Batch(r.ran(), a.withActiveWorktree(cwd, func(active *Worktree, root string, err error) tea.Cmd {
						if err != nil {
							return failed(err)
						}
						r.sys(strings.Join(worktreeStatusLines(ctx, active, root != ""), "\n"))
						return nil
					}))
				case "new":
					return tea.Batch(r.ran(), a.worktreeNew(r, cwd, rest))
				case "list":
					return tea.Batch(r.ran(), call(a.gw, tuigateway.WorktreeList, tuigateway.WorktreeCwdParams{Cwd: cwd}, func(res *tuigateway.WorktreeListResult, err error) tea.Cmd {
						if err != nil {
							return failed(err)
						}
						lines := res.Lines
						switch {
						case res.RepoRoot == nil:
							r.sys(commandText(ctx, "worktree.not_in_repo", nil))
							return nil
						case len(lines) == 0:
							lines = []string{commandText(ctx, "worktree.list_failed", nil)}
						}
						return a.showOutput(strings.Join(lines, "\n"), "worktree")
					}))
				}
				dryRun := worktreeDryRun(rest)
				return tea.Batch(r.ran(), a.withActiveWorktree(cwd, func(active *Worktree, root string, err error) tea.Cmd {
					switch {
					case err != nil:
						return failed(err)
					case root == "":
						r.sys(commandText(ctx, "worktree.not_in_repo", nil))
						return nil
					}
					params := tuigateway.WorktreePruneParams{Cwd: cwd, DryRun: dryRun}
					if active != nil {
						params.Keep = active.Path
					}
					return call(a.gw, tuigateway.WorktreePrune, params, func(res *tuigateway.WorktreePruneResult, err error) tea.Cmd {
						if err != nil {
							return failed(err)
						}
						return a.showOutput(strings.Join(worktreePruneLines(ctx, res, dryRun), "\n"), "worktree")
					})
				}))
			}},
	}
}
