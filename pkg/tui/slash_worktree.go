package tui

import (
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"context"
	"os"
	"strings"
	"sync"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/i18n"
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

// Tree of a worktree answer, false when the answer names none
func worktreeOf(v any) (Worktree, bool) {
	m, _ := v.(map[string]any)
	path, _ := m["path"].(string)
	branch, _ := m["branch"].(string)
	root, _ := m["repo_root"].(string)
	return Worktree{Path: path, Branch: branch, RepoRoot: root}, path != ""
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
func worktreePruneLines(ctx context.Context, res map[string]any, dryRun bool) []string {
	if failure, failed := res["error"].(string); failed {
		return []string{"error: " + failure}
	}
	lines := rpcStrings(res["actions"])
	if len(lines) > 0 {
		key := "worktree.actions_done"
		if dryRun {
			key = "worktree.actions_planned"
		}
		lines = append(lines, commandText(ctx, key, map[string]any{"count": len(lines)}))
	} else {
		lines = append(lines, commandText(ctx, "worktree.nothing_to_reclaim", nil))
	}
	preserved, _ := res["preserved"].([]any)
	if len(preserved) > 0 {
		lines = append(lines, commandText(ctx, "worktree.preserved", map[string]any{"count": len(preserved)}))
		for _, item := range preserved {
			kept, _ := item.(map[string]any)
			lines = append(lines, "  "+jsValueString(kept["name"])+": "+jsValueString(kept["reason"]))
		}
	}
	return lines
}

// Asks the gateway for the folder's repository and the tree a -w launch of this process locked
func (a *app) worktreeStatus(cwd string, then func(repoRoot string, launch *Worktree, err error) tea.Cmd) tea.Cmd {
	params := map[string]any{"cwd": cwd, "launch_cwd": os.Getenv("ICEBATH_CWD"), "pid": os.Getpid()}
	return gatewayCall(a.gw, "worktree.status", params, func(v any, err error) tea.Cmd {
		res := asRPCResult(v)
		root, _ := res["repo_root"].(string)
		var launch *Worktree
		if tree, named := worktreeOf(res["launch"]); named {
			launch = &tree
		}
		return then(root, launch, err)
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
	return gatewayCall(a.gw, "worktree.create", map[string]any{"cwd": cwd, "name": name, "pid": os.Getpid()}, func(v any, err error) tea.Cmd {
		if err != nil {
			r.sys("error: " + rpcErrorMessage(err))
			return nil
		}
		res := asRPCResult(v)
		if _, inRepo := res["repo_root"].(string); !inRepo {
			r.sys(commandText(a.ctx, "worktree.new_requires_repo", nil))
			return nil
		}
		if lines := rpcStrings(res["lines"]); len(lines) > 0 {
			r.sys(strings.Join(lines, "\n"))
		}
		tree, made := worktreeOf(res["tree"])
		if !made {
			return nil
		}
		return a.enterWorktree(r, tree)
	})
}

// Moves the session's workspace into the tree through the gateway, then this process and later sessions follow
func (a *app) enterWorktree(r *slashRun, tree Worktree) tea.Cmd {
	ctx := a.ctx
	ref := map[string]any{"path": tree.Path, "branch": tree.Branch, "repo_root": tree.RepoRoot}
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
		return gatewayCall(a.gw, "worktree.record", ref, func(_ any, err error) tea.Cmd {
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
func (a *app) settleRefusedWorktree(r *slashRun, ref map[string]any) tea.Cmd {
	return gatewayCall(a.gw, "worktree.settle", ref, func(v any, err error) tea.Cmd {
		switch {
		case err != nil:
			r.sys("error: " + rpcErrorMessage(err))
		case len(rpcStrings(asRPCResult(v)["lines"])) > 0:
			r.sys(strings.Join(rpcStrings(asRPCResult(v)["lines"]), "\n"))
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
					return tea.Batch(r.ran(), gatewayCall(a.gw, "worktree.list", map[string]any{"cwd": cwd}, func(v any, err error) tea.Cmd {
						res := asRPCResult(v)
						lines := rpcStrings(res["lines"])
						switch _, inRepo := res["repo_root"].(string); {
						case err != nil:
							return failed(err)
						case !inRepo:
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
					params := map[string]any{"cwd": cwd, "dry_run": dryRun, "keep": nil}
					if active != nil {
						params["keep"] = active.Path
					}
					return gatewayCall(a.gw, "worktree.prune", params, func(v any, err error) tea.Cmd {
						if err != nil {
							return failed(err)
						}
						return a.showOutput(strings.Join(worktreePruneLines(ctx, asRPCResult(v), dryRun), "\n"), "worktree")
					})
				}))
			}},
	}
}
