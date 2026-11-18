package tui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/internal/pysys"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Trees /worktree new created in this process, the newest the active one, each settled on quit
var newWorktrees struct {
	sync.Mutex
	active  map[string]string
	created []map[string]string
}

// Subcommands of /worktree and the handler each runs
var worktreeHandlers = map[string]string{"prune": "prune", "gc": "prune", "clean": "prune", "list": "list", "ls": "list",
	"new": "new", "add": "new", "create": "new"}

// Held for a whole capture so two never swap the process streams out of order
var stdioCapture sync.Mutex

// Runs fn with the process stdout and stderr captured, returning what it printed without escapes
func captureStdio(fn func()) string {
	stdioCapture.Lock()
	defer stdioCapture.Unlock()
	var buf bytes.Buffer
	var mu sync.Mutex
	w := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	})
	restore := pysys.Patch(w, w)
	defer restore()
	fn()
	return strings.TrimRight(tools.StripANSI(buf.String()), "\n")
}

// Writer of a function
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// Tree this process locked with its -w launch, nil when the launch made none
func launchWorktree(cwd string) map[string]string {
	if cwd == "" {
		return nil
	}
	res, err := pysubprocess.RunWith(context.Background(), []string{"git", "worktree", "list", "--porcelain"}, pysubprocess.Options{Dir: cwd, Timeout: 10 * time.Second})
	if err != nil || res.Returncode != 0 {
		return nil
	}
	text, _ := pycodecs.DecodeReplace(res.Stdout, "utf-8")
	owner := fmt.Sprintf("icebath pid=%d", os.Getpid())
	for _, block := range strings.Split(text, "\n\n") {
		entry := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			key, value, _ := strings.Cut(line, " ")
			entry[key] = value
		}
		reason, locked := entry["locked"]
		if !locked || (reason != owner && !strings.HasPrefix(reason, owner+" ")) {
			continue
		}
		return map[string]string{"path": entry["worktree"], "branch": strings.TrimPrefix(entry["branch"], "refs/heads/")}
	}
	return nil
}

// Tree the session works in, a /worktree new tree first, then the tree of a -w launch
func (a *app) activeWorktree() map[string]string {
	newWorktrees.Lock()
	active := newWorktrees.active
	newWorktrees.Unlock()
	if active != nil {
		return active
	}
	return launchWorktree(os.Getenv("ICEBATH_CWD"))
}

// Lines of the status form, the active tree or none, with the hints when inside a repository
func worktreeStatusLines(ctx context.Context, active map[string]string, repoRoot string) []string {
	var lines []string
	if active != nil {
		lines = append(lines, commandText(ctx, "worktree.active", map[string]any{"path": active["path"]}),
			commandText(ctx, "worktree.branch", map[string]any{"branch": active["branch"]}))
	} else {
		lines = append(lines, commandText(ctx, "worktree.none_active", nil))
	}
	if repoRoot != "" {
		return append(lines, commandText(ctx, "worktree.hint_new", nil), commandText(ctx, "worktree.hint_prune", nil))
	}
	return append(lines, commandText(ctx, "worktree.not_in_repo_note", nil))
}

// Lines of git worktree list for the repository, the list failed line when git fails
func worktreeListLines(ctx context.Context, repoRoot string) []string {
	res, err := pysubprocess.RunWith(ctx, []string{"git", "worktree", "list"}, pysubprocess.Options{Dir: repoRoot, Timeout: 10 * time.Second})
	out := ""
	if err == nil && res.Returncode == 0 {
		text, _ := pycodecs.DecodeReplace(res.Stdout, "utf-8")
		out = pystr.Strip(text)
	}
	if out == "" {
		return []string{commandText(ctx, "worktree.list_failed", nil)}
	}
	return pystr.Splitlines(out, false)
}

// Lines of a prune of stale trees and merged branches, never the active tree
func worktreePruneLines(ctx context.Context, repoRoot, rest string, active map[string]string) ([]string, error) {
	rest = pystr.Lower(rest)
	dryRun := strings.Contains(rest, "--dry-run")
	for _, word := range pystr.SplitWhitespace(rest, -1) {
		dryRun = dryRun || word == "-n"
	}
	var lines []string
	var failure error
	printed := captureStdio(func() {
		records, err := tools.AuditWorktrees(ctx, repoRoot, false, nil)
		if err != nil {
			failure = err
			return
		}
		kept := []tools.TreeRecord{}
		for _, r := range records {
			if active == nil || r.Path != active["path"] {
				kept = append(kept, r)
			}
		}
		actions, err := tools.ReclaimWorktrees(ctx, repoRoot, dryRun, kept)
		if err != nil {
			failure = err
			return
		}
		branchActions, err := tools.ReclaimBranches(repoRoot, dryRun, nil)
		if err != nil {
			failure = err
			return
		}
		actions = append(actions, branchActions...)
		if len(actions) > 0 {
			key := "worktree.actions_done"
			if dryRun {
				key = "worktree.actions_planned"
			}
			lines = append(append(lines, actions...), commandText(ctx, key, map[string]any{"count": len(actions)}))
		} else {
			lines = append(lines, commandText(ctx, "worktree.nothing_to_reclaim", nil))
		}
		var preserved []tools.TreeRecord
		for _, r := range kept {
			if r.Verdict == "keep" && !strings.Contains(r.Reason, "kanban") && !strings.Contains(r.Reason, "in use") {
				preserved = append(preserved, r)
			}
		}
		if len(preserved) > 0 {
			lines = append(lines, commandText(ctx, "worktree.preserved", map[string]any{"count": len(preserved)}))
			for _, r := range preserved {
				lines = append(lines, "  "+r.Name+": "+r.Reason)
			}
		}
	})
	if printed != "" {
		lines = append(strings.Split(printed, "\n"), lines...)
	}
	return lines, failure
}

// Whether the profile syncs a new tree with its upstream base, on unless worktree_sync says otherwise
func worktreeSyncBase(ctx context.Context) bool {
	h := home.Dir(ctx)
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return true
	}
	if v, ok := cfg["worktree_sync"]; ok {
		return pyrepr.Truthy(v)
	}
	return true
}

// Creates a tree and moves the session into it, the printed creation lines first
func (a *app) worktreeNew(r *slashRun, repoRoot, rest string) tea.Cmd {
	ctx := a.ctx
	return func() tea.Msg {
		var info map[string]string
		printed := captureStdio(func() { info = tools.SetupWorktree(ctx, repoRoot, worktreeSyncBase(ctx), rest, nil) })
		return asyncDoneMsg{run: func() tea.Cmd {
			if printed != "" {
				r.sys(printed)
			}
			if len(info) == 0 {
				return nil
			}
			return a.enterWorktree(r, info)
		}}
	}
}

// Moves the session's workspace into the tree through the gateway, then this process and later sessions follow
func (a *app) enterWorktree(r *slashRun, info map[string]string) tea.Cmd {
	ctx := a.ctx
	entered := func() tea.Cmd {
		newWorktrees.Lock()
		newWorktrees.active = info
		newWorktrees.created = append(newWorktrees.created, info)
		newWorktrees.Unlock()
		lines := []string{commandText(ctx, "worktree.ready", map[string]any{"path": info["path"]}),
			commandText(ctx, "worktree.branch", map[string]any{"branch": info["branch"]}), commandText(ctx, "worktree.tools_in_worktree", nil)}
		if err := a.followWorktree(info["path"]); err != nil {
			lines = append(lines, i18n.T(ctx, "cli.session.cwd_enter_failed", "", map[string]any{"path": info["path"], "error": err.Error()}))
		}
		r.sys(strings.Join(lines, "\n"))
		return func() tea.Msg {
			tools.RecordTUIWorktree(ctx, info)
			return nil
		}
	}
	settle := func() tea.Msg {
		var echoed bytes.Buffer
		printed := captureStdio(func() { settleWorktree(ctx, info, &echoed) })
		shown := strings.TrimSpace(strings.Join([]string{printed, tools.StripANSI(echoed.String())}, "\n"))
		return asyncDoneMsg{run: func() tea.Cmd {
			if shown != "" {
				r.sys(shown)
			}
			return nil
		}}
	}
	refused := func(err error) tea.Cmd {
		r.sys(commandText(ctx, "worktree.enter_failed", map[string]any{"error": rpcErrorMessage(err)}))
		return settle
	}
	if r.sid == "" {
		return entered()
	}
	if a.gw.canonical() {
		return a.store.mutateCanonical(a.gw, r.sid, "workspace", map[string]any{"cwd": info["path"]}, r.stale, func(m *mutationOutcome, err error) tea.Cmd {
			switch {
			case err != nil:
				return refused(err)
			case m == nil:
				return settle
			}
			return entered()
		})
	}
	key := a.store.ui.StoredSid
	if key == "" {
		key = r.sid
	}
	return gatewayCall(a.gw, "session.workspace.move", map[string]any{"session_key": key, "cwd": info["path"]}, func(_ any, err error) tea.Cmd {
		if err != nil {
			return refused(err)
		}
		return entered()
	})
}

// Points this process, its terminal cwd and the sessions it creates next at the tree
func (a *app) followWorktree(path string) error {
	if info := a.store.ui.Info; info != nil {
		info["cwd"] = path
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

// Removes a clean tree and its branch, keeping one that holds uncommitted, unpushed or embedded work
func settleWorktree(ctx context.Context, info map[string]string, out io.Writer) {
	path, branch, repoRoot := info["path"], info["branch"], info["repo_root"]
	if _, err := os.Stat(path); err != nil {
		return
	}
	newWorktrees.Lock()
	if newWorktrees.active != nil && newWorktrees.active["path"] == path {
		newWorktrees.active = nil
	}
	newWorktrees.Unlock()
	echo := func(text string) { fmt.Fprintln(out, text) }
	anyInfo := map[string]any{}
	for k, v := range info {
		anyInfo[k] = v
	}
	if tools.ApplyExitChoice(ctx, anyInfo, echo) {
		return
	}
	text := func(key string) string { return i18n.T(ctx, "cli.worktree."+key, "", map[string]any{"path": path}) }
	switch {
	case tools.WorktreeIsDirty(path, repoRoot, 10*time.Second):
		echo("\n\x1b[33m" + text("uncommitted_keeping") + "\x1b[0m")
		return
	case tools.WorktreeHasUnpushedCommits(path, 10*time.Second) && tools.RepoIsShallow(repoRoot, 10*time.Second):
		echo("\n\x1b[33m" + text("shallow_clone_keeping") + "\x1b[0m")
		echo("  " + text("next_session_deepens"))
		return
	case tools.WorktreeHasUnpushedCommits(path, 10*time.Second):
		echo("\n\x1b[33m" + text("unpushed_keeping") + "\x1b[0m")
		echo("  " + text("clean_up_manually"))
		return
	}
	if blocked := tools.RemovalBlocked(path); blocked != nil {
		echo("\n\x1b[33m" + i18n.T(ctx, "cli.worktree.kept_reason", "", map[string]any{"path": path, "reason": blocked.Reason}) + "\x1b[0m")
		return
	}
	tools.ReleaseLspClients(path)
	tools.WorktreeGitQuiet([]string{"worktree", "unlock", path}, repoRoot, 10*time.Second, "git worktree unlock failed (non-fatal)", nil)
	tools.WorktreeGitQuiet([]string{"worktree", "remove", path, "--force"}, repoRoot, 15*time.Second, "Failed to remove worktree", nil)
	tools.WorktreeGitQuiet([]string{"branch", "-D", branch}, repoRoot, 10*time.Second, fmt.Sprintf("Failed to delete branch %s", branch), nil)
	echo("\x1b[32m" + text("cleaned_up") + "\x1b[0m")
}

// Settles every tree /worktree new made once the terminal UI is gone, asking about the active one first
func settleNewWorktrees(ctx context.Context, out io.Writer) {
	newWorktrees.Lock()
	active, created := newWorktrees.active, append([]map[string]string(nil), newWorktrees.created...)
	newWorktrees.Unlock()
	for i := len(created) - 1; i >= 0; i-- {
		info := created[i]
		if active != nil && info["path"] == active["path"] {
			anyInfo := map[string]any{}
			for k, v := range info {
				anyInfo[k] = v
			}
			tools.AskExitChoice(ctx, anyInfo)
			if choice, _ := anyInfo["exit_choice"].(string); choice != "" {
				info = copyWith(info, "exit_choice", choice)
			}
		}
		settleWorktree(ctx, info, out)
	}
}

// Copy of a string map with one more key
func copyWith(m map[string]string, key, value string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out[key] = value
	return out
}

// Slash command of isolated git worktrees, status, list, new and prune
func (a *app) worktreeCommands() []slashCommand {
	return []slashCommand{
		{name: "worktree", help: "show, list, create, or prune isolated git worktrees", usage: "[new [name]|list|prune [--dry-run]]",
			suggest: func(string) []string { return []string{"new", "list", "prune"} },
			run: func(r *slashRun) tea.Cmd {
				parts := pystr.SplitWhitespace(r.cmd, 2)
				sub, rest := "", ""
				if len(parts) > 1 {
					sub = pystr.Lower(parts[1])
				}
				if len(parts) > 2 {
					rest = pystr.Strip(parts[2])
				}
				ctx, cwd := a.ctx, a.store.sessionCwd()
				handler, known := worktreeHandlers[sub]
				if sub == "" || sub == "status" || sub == "show" {
					handler, known = "status", true
				}
				if !known {
					r.sys(commandText(ctx, "worktree.unknown_subcommand", map[string]any{"subcommand": sub}) + "\n" + commandText(ctx, "worktree.usage", nil))
					return r.ran()
				}
				// Git lookups and tree work run here off the update loop, their lines shown back on it
				return tea.Batch(r.ran(), func() tea.Msg {
					repoRoot := tools.GitRepoRootOf(cwd)
					var lines []string
					switch {
					case handler == "status":
						lines = worktreeStatusLines(ctx, a.activeWorktree(), repoRoot)
					case repoRoot == "" && handler == "new":
						lines = []string{commandText(ctx, "worktree.new_requires_repo", nil)}
					case repoRoot == "":
						lines = []string{commandText(ctx, "worktree.not_in_repo", nil)}
					case handler == "new":
						return a.worktreeNew(r, repoRoot, rest)()
					default:
						var out []string
						if handler == "list" {
							out = worktreeListLines(ctx, repoRoot)
						} else if pruned, err := worktreePruneLines(ctx, repoRoot, rest, a.activeWorktree()); err != nil {
							out = append(pruned, "error: "+err.Error())
						} else {
							out = pruned
						}
						return asyncDoneMsg{run: func() tea.Cmd { return a.showOutput(strings.Join(out, "\n"), "worktree") }}
					}
					return asyncDoneMsg{run: func() tea.Cmd {
						r.sys(strings.Join(lines, "\n"))
						return nil
					}}
				})
			}},
	}
}
