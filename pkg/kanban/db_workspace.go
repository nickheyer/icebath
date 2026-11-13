package kanban

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"golang.org/x/text/unicode/norm"
)

var removableKinds = map[string]bool{"scratch": true, "worktree": true}

// NFC form of a path string so Unicode form never decides a path identity question
func pathKey(p string) string { return norm.NFC.String(p) }

const terminalStatusesSQL = "('done', 'archived', 'failed', 'cancelled')"

const activeChildrenSQL = "SELECT 1 FROM task_links l JOIN tasks t ON t.id = l.child_id " +
	"WHERE l.parent_id = ? AND t.status NOT IN " + terminalStatusesSQL + " LIMIT 1"

const workspaceRowSQL = "SELECT workspace_kind, workspace_path, branch_name FROM tasks WHERE id = ?"

const terminalTaskSQL = "SELECT 1 FROM tasks WHERE id = ? AND status IN " + terminalStatusesSQL

const otherLivePathsSQL = "SELECT workspace_path FROM tasks WHERE id != ? AND workspace_path IS NOT NULL " +
	"AND status NOT IN " + terminalStatusesSQL

const anyLivePathsSQL = "SELECT workspace_path FROM tasks WHERE workspace_path IS NOT NULL " +
	"AND status NOT IN " + terminalStatusesSQL

// gitResult is one finished git call with its exit status and decoded streams
type gitResult struct {
	Returncode     int
	Stdout, Stderr string
}

// Environment list of a map in key order
func environOf(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

// Runs git -C repoRoot args under the noninteractive repo env, a non zero exit returned rather than raised
func wsGit(repoRoot string, timeout time.Duration, args ...string) (*gitResult, error) {
	env := tools.NoninteractiveRepoGitEnv(repoRoot, nil)
	if env == nil {
		return &gitResult{Returncode: 1, Stderr: tools.GitFilterDiscoveryFailed}, nil
	}
	argv := append([]string{"git", "-C", repoRoot}, args...)
	r, err := pysubprocess.RunWith(context.Background(), argv, pysubprocess.Options{Env: environOf(env), Timeout: timeout})
	if err != nil {
		return nil, err
	}
	return &gitResult{r.Returncode, pystr.TranslateNewlines(pystr.DecodeUTF8Replace(r.Stdout)),
		pystr.TranslateNewlines(pystr.DecodeUTF8Replace(r.Stderr))}, nil
}

// Stripped stderr, or stdout when stderr is empty
func (r *gitResult) failureText() string {
	if r.Stderr != "" {
		return pystr.Strip(r.Stderr)
	}
	return pystr.Strip(r.Stdout)
}

// Reports whether a path is an existing directory the way Path.is_dir does
func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Reports whether a path is an existing regular file the way Path.is_file does
func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// Python str of an exception for log lines, by sqlite, OSError and generic class
func excText(err error) string {
	if _, ok := state.SQLiteClassOf(err); ok {
		return state.ErrText(err)
	}
	if pypath.IsOSError(err) {
		return pypath.OSErrorText(err)
	}
	return err.Error()
}

// Reports whether an error is the sqlite3.Error class
func isSQLiteError(err error) bool {
	_, ok := state.SQLiteClassOf(err)
	return ok
}

// Reports whether any child of the task is still non terminal
func hasActiveChildren(ctx context.Context, conn state.SQLiteConn, taskID string) (bool, error) {
	row, err := QueryOne(ctx, conn, activeChildrenSQL, taskID)
	return row != nil, err
}

// Reports whether the task sits in a terminal status
func isTerminalTask(ctx context.Context, conn state.SQLiteConn, taskID string) (bool, error) {
	row, err := QueryOne(ctx, conn, terminalTaskSQL, taskID)
	return row != nil, err
}

// Reports whether a live task on this connection resolves its workspace to key
func connUsesPath(ctx context.Context, conn state.SQLiteConn, taskID, key string, excludeTaskID bool) (bool, error) {
	var rows []*Row
	var err error
	if excludeTaskID {
		rows, err = QueryRows(ctx, conn, otherLivePathsSQL, taskID)
	} else {
		rows, err = QueryRows(ctx, conn, anyLivePathsSQL)
	}
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		other := row.Get("workspace_path")
		if !truthy(other) {
			continue
		}
		expanded, err := pypath.PathExpanduser(cellStr(other))
		if err != nil {
			return false, err
		}
		resolved, err := pypath.Resolve(expanded)
		if err != nil {
			continue
		}
		if pathKey(resolved) == key {
			return true, nil
		}
	}
	return false, nil
}

// Every other board database file from the on disk layout, an OSError when the set is unknown
func siblingBoardDBFiles(ctx context.Context, conn state.SQLiteConn) ([]string, error) {
	current := ""
	if currentFile := mainDBFile(ctx, conn); currentFile != "" {
		r, err := pypath.Resolve(currentFile)
		if err != nil {
			return nil, err
		}
		current = r
	}
	candidates := []string{pypath.PathJoin(KanbanHome(), "kanban.db")}
	root := BoardsRoot()
	if isDir(root) {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			child := pypath.PathJoin(root, e.Name())
			if isDir(child) {
				candidates = append(candidates, pypath.PathJoin(child, "kanban.db"))
			}
		}
	}
	var found []string
	seen := map[string]bool{}
	for _, p := range candidates {
		if !isFile(p) {
			continue
		}
		resolved, err := pypath.Resolve(p)
		if err != nil {
			return nil, err
		}
		if (current != "" && resolved == current) || seen[resolved] {
			continue
		}
		seen[resolved] = true
		found = append(found, resolved)
	}
	return found, nil
}

// Reports whether a live task of another board database uses the workspace key
func otherBoardUsesPath(ctx context.Context, dbFile, taskID, key string) (bool, error) {
	uri, err := pypath.AsURI(dbFile)
	if err != nil {
		return false, err
	}
	other, err := state.ConnectSQLite(uri+"?mode=ro", state.SQLiteConfig{Timeout: 1.0, URI: true})
	if err != nil {
		return false, err
	}
	defer other.Close()
	return connUsesPath(ctx, other, taskID, key, false)
}

// Why a workspace must be kept, shared or unknown, nil when no live task uses it
func workspaceInUseByOther(ctx context.Context, conn state.SQLiteConn, taskID, path string) (*string, error) {
	unknown, shared := "unknown", "shared"
	expanded, err := pypath.PathExpanduser(path)
	if err != nil {
		return nil, err
	}
	resolved, err := pypath.Resolve(expanded)
	if err != nil {
		log.Warn(fmt.Sprintf("Cannot resolve workspace %s for task %s: %s", path, taskID, excText(err)))
		return &unknown, nil
	}
	key := pathKey(resolved)
	if key == "" {
		return nil, nil
	}
	used, err := connUsesPath(ctx, conn, taskID, key, true)
	if err != nil {
		if !isSQLiteError(err) {
			return nil, err
		}
		log.Warn(fmt.Sprintf("Cannot read live workspaces for task %s: %s", taskID, excText(err)))
		return &unknown, nil
	}
	if used {
		return &shared, nil
	}
	siblings, err := siblingBoardDBFiles(ctx, conn)
	if err != nil {
		log.Warn(fmt.Sprintf("Cannot list kanban boards for task %s: %s", taskID, excText(err)))
		return &unknown, nil
	}
	for _, dbFile := range siblings {
		used, err := otherBoardUsesPath(ctx, dbFile, taskID, key)
		if err != nil {
			log.Warn(fmt.Sprintf("Cannot read board db %s for task %s: %s", dbFile, taskID, excText(err)))
			return &unknown, nil
		}
		if used {
			return &shared, nil
		}
	}
	return nil, nil
}

// Skips removal and records why when the path may still be used by a live task
func deferSharedWorkspaceCleanup(ctx context.Context, conn state.SQLiteConn, taskID, path string) (bool, error) {
	reason, err := workspaceInUseByOther(ctx, conn, taskID, path)
	if err != nil {
		return false, err
	}
	if reason == nil {
		return false, nil
	}
	if *reason == "shared" {
		log.Warn(fmt.Sprintf("Deferring workspace cleanup for task %s: %s is still used by another non-terminal task", taskID, path))
	} else {
		log.Warn(fmt.Sprintf("Deferring workspace cleanup for task %s: cannot tell whether another task still uses %s", taskID, path))
	}
	if err := appendEvent(ctx, conn, taskID, "workspace_cleanup_deferred_shared", payload("path", path, "reason", *reason), nil); err != nil && !isSQLiteError(err) {
		return true, err
	}
	return true, nil
}

// Defers removing a linked worktree another live task may still use
func deferSharedWorktreeCleanup(ctx context.Context, conn state.SQLiteConn, taskID, path string) (bool, error) {
	wt, err := pypath.PathExpanduser(path)
	if err != nil {
		return false, err
	}
	if !isDir(wt) {
		return false, nil
	}
	linked, err := isLinkedWorktreeCheckout(wt)
	if err != nil || !linked {
		return false, err
	}
	return deferSharedWorkspaceCleanup(ctx, conn, taskID, path)
}

// Absolute dot dot collapsed NFC form of a path without following symlinks
func lexicalPath(p string) (string, error) {
	abs, err := pypath.Abspath(p)
	if err != nil {
		return "", err
	}
	return pypath.PathStr(pathKey(abs)), nil
}

// scratchRoot is one managed workspaces root with its resolved and lexical spellings
type scratchRoot struct {
	resolved string
	lexical  []string
	board    *string
}

// Reports whether a path is a strict descendant of a managed workspaces root, with the matching board
func managedScratchPathInfo(p string) (bool, *string, error) {
	pAbs, err := pypath.Resolve(p)
	if err != nil {
		return false, nil, nil
	}
	pLex, err := lexicalPath(p)
	if err != nil {
		return false, nil, err
	}
	var roots []scratchRoot
	addRoot := func(anchor, anchorReal string, parts []string, board *string) {
		root := pypath.PathJoin(anchor, parts...)
		resolved, err := pypath.Resolve(root)
		if err != nil {
			return
		}
		lex, err := lexicalPath(root)
		if err != nil {
			return
		}
		lexReal, err := lexicalPath(pypath.PathJoin(anchorReal, parts...))
		if err != nil {
			return
		}
		roots = append(roots, scratchRoot{resolved, []string{lex, lexReal}, board})
	}
	if override := strings.TrimSpace(os.Getenv("ICEBATH_KANBAN_WORKSPACES_ROOT")); override != "" {
		overrideRoot, err := pypath.PathExpanduser(override)
		if err != nil {
			return false, nil, err
		}
		overrideParent := pypath.PathParent(overrideRoot)
		if parentReal, err := pypath.Resolve(overrideParent); err == nil {
			addRoot(overrideParent, parentReal, []string{pypath.PathName(overrideRoot)}, nil)
		}
	}
	home := KanbanHome()
	if homeReal, err := pypath.Resolve(home); err == nil {
		def := DefaultBoard
		addRoot(home, homeReal, []string{"kanban", "workspaces"}, &def)
		if boards, err := pypath.Resolve(pypath.PathJoin(home, "kanban", "boards")); err == nil {
			if entries, err := os.ReadDir(boards); err == nil {
				for _, e := range entries {
					if isDir(pypath.PathJoin(boards, e.Name())) {
						name := e.Name()
						addRoot(home, homeReal, []string{"kanban", "boards", name, "workspaces"}, &name)
					}
				}
			}
		}
	}
	for _, r := range roots {
		if pAbs == r.resolved {
			continue
		}
		if _, ok := pypath.PathRelativeTo(pAbs, r.resolved); !ok {
			continue
		}
		for _, lex := range r.lexical {
			if pLex == lex {
				continue
			}
			if _, ok := pypath.PathRelativeTo(pLex, lex); ok {
				return true, r.board, nil
			}
		}
	}
	return false, nil, nil
}

// Expanded workspace path of a scratch task, nil for any other kind or an empty path
func scratchWorkspace(ctx context.Context, conn state.SQLiteConn, taskID string) (*string, error) {
	row, err := QueryOne(ctx, conn, "SELECT workspace_kind, workspace_path FROM tasks WHERE id = ?", taskID)
	if err != nil || row == nil {
		return nil, err
	}
	if row.Get("workspace_kind") != "scratch" || !truthy(row.Get("workspace_path")) {
		return nil, nil
	}
	p, err := pypath.PathExpanduser(cellStr(row.Get("workspace_path")))
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// Reports whether a path is a strict descendant of a kanban managed workspaces root
func isManagedScratchPath(p string) (bool, error) {
	ok, _, err := managedScratchPathInfo(p)
	return ok, err
}

// Removes a tree ignoring every error like shutil.rmtree with ignore_errors
func rmtreeIgnoreErrors(p string) {
	_ = pyfs.RmTree(p, func(pyfs.RmTreeOp, string, error) error { return nil })
}

// Removes a finished task workspace when managed and free, every failure swallowed so completion never blocks
func cleanupWorkspace(ctx context.Context, conn state.SQLiteConn, taskID string) {
	_ = cleanupWorkspaceChecked(ctx, conn, taskID)
}

// Workspace cleanup with its first failure returned, scratch removed, worktree removed when free of work, dir kept
func cleanupWorkspaceChecked(ctx context.Context, conn state.SQLiteConn, taskID string) error {
	row, err := QueryOne(ctx, conn, workspaceRowSQL, taskID)
	if err != nil || row == nil {
		return err
	}
	kind := cellOptStr(row.Get("workspace_kind"))
	path := cellOptStr(row.Get("workspace_path"))
	if kind == nil || !removableKinds[*kind] || path == nil || *path == "" {
		tryCleanupParentWorkspaces(ctx, conn, taskID)
		return nil
	}
	active, err := hasActiveChildren(ctx, conn, taskID)
	if err != nil {
		return err
	}
	if active {
		log.Debug(fmt.Sprintf("Deferring %s workspace cleanup for task %s: active children still need workspace at %s", *kind, taskID, *path))
		return nil
	}
	if *kind == "worktree" {
		cleanupWorkerTmux(ctx, conn, taskID)
		deferred, err := deferSharedWorktreeCleanup(ctx, conn, taskID, *path)
		if err != nil {
			return err
		}
		if !deferred {
			cleanupWorktreeWorkspace(taskID, *path, cellOptStr(row.Get("branch_name")))
		}
		tryCleanupParentWorkspaces(ctx, conn, taskID)
		return nil
	}
	wp := pypath.PathStr(*path)
	if isDir(wp) {
		managed, err := isManagedScratchPath(wp)
		if err != nil {
			return err
		}
		if managed {
			deferred, err := deferSharedWorkspaceCleanup(ctx, conn, taskID, *path)
			if err != nil {
				return err
			}
			if !deferred {
				tools.ReleaseLspClients(wp)
				rmtreeIgnoreErrors(wp)
				log.Debug(fmt.Sprintf("Removed scratch workspace: %s", wp))
			}
		} else {
			log.Warn(fmt.Sprintf("Refusing to remove out-of-scratch workspace for task %s: %s "+
				"(workspace_kind='scratch' but path is outside any kanban-managed workspaces root)", taskID, wp))
		}
	}
	cleanupWorkerTmux(ctx, conn, taskID)
	tryCleanupParentWorkspaces(ctx, conn, taskID)
	return nil
}

// Removes a finished task linked worktree when clean and fully pushed, every failure swallowed
func cleanupWorktreeWorkspace(taskID, path string, branchName *string) {
	_ = cleanupWorktreeWorkspaceChecked(taskID, path, branchName)
}

// Worktree removal with its first failure returned, any doubt preserving the tree
func cleanupWorktreeWorkspaceChecked(taskID, path string, branchName *string) error {
	wp, err := pypath.PathExpanduser(path)
	if err != nil {
		return err
	}
	if !isDir(wp) {
		return nil
	}
	common, err := gitCommonDir(wp)
	if err != nil {
		return err
	}
	if common == nil || pypath.PathName(*common) != ".git" {
		return nil
	}
	repoRoot := pypath.PathParent(*common)
	worktreePath, err := pypath.Resolve(wp)
	if err != nil {
		return err
	}
	rootResolved, err := pypath.Resolve(repoRoot)
	if err != nil {
		return err
	}
	if pathKey(worktreePath) == pathKey(rootResolved) {
		return nil
	}
	if tools.WorktreeIsDirty(wp, repoRoot, 10*time.Second) || tools.WorktreeHasUnpushedCommits(wp, 10*time.Second) {
		log.Info(fmt.Sprintf("Preserving worktree for task %s: dirty or unpushed work at %s", taskID, wp))
		return nil
	}
	cwd, cwdKnown := "", false
	if raw, err := pypath.Getcwd(); err == nil {
		if r, err := pypath.Resolve(raw); err == nil {
			cwd, cwdKnown = r, true
		}
	}
	_, inside := pypath.PathRelativeTo(cwd, worktreePath)
	if !cwdKnown || cwd == worktreePath || inside {
		if err := os.Chdir(repoRoot); err != nil {
			shown := "<deleted cwd>"
			if cwdKnown {
				shown = cwd
			}
			log.Warn(fmt.Sprintf("Preserving worktree for task %s: cannot leave %s for %s: %s", taskID, shown, repoRoot, excText(err)))
			return nil
		}
	}
	tools.ReleaseLspClients(worktreePath)
	result, err := wsGit(repoRoot, 60*time.Second, "worktree", "remove", wp)
	if err != nil {
		return err
	}
	if result.Returncode != 0 {
		time.Sleep(100 * time.Millisecond)
		if result, err = wsGit(repoRoot, 60*time.Second, "worktree", "remove", wp); err != nil {
			return err
		}
	}
	if result.Returncode != 0 {
		log.Warn(fmt.Sprintf("git worktree remove failed for task %s at %s: %s", taskID, wp, result.failureText()))
		return nil
	}
	log.Debug(fmt.Sprintf("Removed worktree workspace: %s", wp))
	branch := ""
	if branchName != nil {
		branch = pystr.Strip(*branchName)
	}
	if branch == "" {
		branch = "wt/" + taskID
	}
	if strings.HasPrefix(branch, "wt/") {
		if _, err := wsGit(repoRoot, 30*time.Second, "branch", "-D", branch); err != nil {
			return err
		}
	}
	return nil
}

// Runs the deferred cleanup of every parent workspace whose children are now all terminal
func tryCleanupParentWorkspaces(ctx context.Context, conn state.SQLiteConn, taskID string) {
	rows, err := QueryRows(ctx, conn, "SELECT parent_id FROM task_links WHERE child_id = ?", taskID)
	if err != nil {
		return
	}
	for _, prow := range rows {
		parentID := cellStr(prow.Get("parent_id"))
		row, err := QueryOne(ctx, conn, workspaceRowSQL, parentID)
		if err != nil {
			return
		}
		if row == nil {
			continue
		}
		kind := cellOptStr(row.Get("workspace_kind"))
		wsPath := cellOptStr(row.Get("workspace_path"))
		if kind == nil || !removableKinds[*kind] || wsPath == nil || *wsPath == "" {
			continue
		}
		terminal, err := isTerminalTask(ctx, conn, parentID)
		if err != nil {
			return
		}
		if !terminal {
			continue
		}
		active, err := hasActiveChildren(ctx, conn, parentID)
		if err != nil {
			return
		}
		if active {
			continue
		}
		if *kind == "worktree" {
			deferred, err := deferSharedWorktreeCleanup(ctx, conn, parentID, *wsPath)
			if err != nil {
				return
			}
			if !deferred {
				cleanupWorktreeWorkspace(parentID, *wsPath, cellOptStr(row.Get("branch_name")))
			}
			continue
		}
		wp := pypath.PathStr(*wsPath)
		if !isDir(wp) {
			continue
		}
		managed, err := isManagedScratchPath(wp)
		if err != nil {
			return
		}
		if !managed {
			continue
		}
		deferred, err := deferSharedWorkspaceCleanup(ctx, conn, parentID, *wsPath)
		if err != nil {
			return
		}
		if deferred {
			continue
		}
		tools.ReleaseLspClients(wp)
		rmtreeIgnoreErrors(wp)
		log.Debug(fmt.Sprintf("Deferred cleanup: removed parent %s scratch workspace: %s", parentID, wp))
	}
}

// Kills the tmux session of the task assignee when its pane is dead
func cleanupWorkerTmux(ctx context.Context, conn state.SQLiteConn, taskID string) {
	row, err := QueryOne(ctx, conn, "SELECT assignee FROM tasks WHERE id = ?", taskID)
	if err != nil || row == nil || !truthy(row.Get("assignee")) {
		return
	}
	session := "swarm-" + cellStr(row.Get("assignee"))
	out, err := pysubprocess.RunWith(context.Background(), []string{"tmux", "list-panes", "-t", session, "-F", "#{pane_dead}"}, pysubprocess.Options{Timeout: 5 * time.Second})
	if err != nil {
		return
	}
	if pystr.Strip(pystr.DecodeUTF8Replace(out.Stdout)) == "1" {
		if _, err := pysubprocess.RunWith(context.Background(), []string{"tmux", "kill-session", "-t", session}, pysubprocess.Options{Timeout: 5 * time.Second}); err != nil {
			return
		}
		log.Debug(fmt.Sprintf("Killed stale tmux session: %s", session))
	}
}

const scratchTipSentinelName = ".scratch_tip_shown"

const scratchTipMessage = "scratch workspaces are ephemeral — they're deleted when the task " +
	"completes. Use --workspace worktree: (git worktree) or " +
	"--workspace dir:/abs/path (existing dir) to preserve worker output."

// Per install sentinel file of the scratch workspace tip
func scratchTipSentinelPath() string {
	return pypath.PathJoin(KanbanHome(), scratchTipSentinelName)
}

// Reports whether the scratch workspace tip was already emitted on this install
func scratchTipShown() bool {
	return pathExists(scratchTipSentinelPath())
}

// Touches the sentinel so later scratch workspaces stay silent, a failure left to re emit the tip
func markScratchTipShown() {
	p := scratchTipSentinelPath()
	if err := os.MkdirAll(pypath.PathParent(p), 0o777); err != nil {
		return
	}
	now := time.Now()
	if err := os.Chtimes(p, now, now); err == nil {
		return
	}
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o666); err == nil {
		f.Close()
	}
}

// Emits the first use scratch workspace tip once per install right after a scratch workspace is materialized
func maybeEmitScratchTip(ctx context.Context, conn state.SQLiteConn, taskID string, workspaceKind *string) {
	kind := "scratch"
	if workspaceKind != nil && *workspaceKind != "" {
		kind = *workspaceKind
	}
	if kind != "scratch" || scratchTipShown() {
		return
	}
	defer markScratchTipShown()
	log.Warn(fmt.Sprintf("kanban: %s (task %s)", scratchTipMessage, taskID))
	_ = WriteTxn(ctx, conn, false, func() error {
		return appendEvent(ctx, conn, taskID, "tip_scratch_workspace", payload("message", scratchTipMessage), nil)
	})
}

// Resolved git toplevel containing the path, nil outside a repo
func gitToplevel(path string) *string {
	out := gitOut(path, 30*time.Second, "rev-parse", "--show-toplevel")
	if out == nil {
		return nil
	}
	expanded := pypath.PathStr(pypath.Expanduser(*out))
	resolved, err := pypath.Resolve(expanded)
	if err != nil {
		return &expanded
	}
	return &resolved
}

// Reports whether refs/heads/branchName exists in the repo
func gitBranchExists(repoRoot, branchName string) bool {
	r, err := wsGit(repoRoot, 30*time.Second, "show-ref", "--verify", "refs/heads/"+branchName)
	return err == nil && r.Returncode == 0
}

// Resolved absolute path git rev-parse reports for flag, nil when git fails
func gitAbsPath(path, flag string) (*string, error) {
	out := gitOut(path, 30*time.Second, "rev-parse", "--path-format=absolute", flag)
	if out == nil {
		return nil, nil
	}
	resolved, err := pypath.Resolve(pypath.PathStr(pypath.Expanduser(*out)))
	if err != nil {
		return nil, err
	}
	return &resolved, nil
}

// Resolved common git dir of a checkout, nil outside a repo
func gitCommonDir(path string) (*string, error) {
	return gitAbsPath(path, "--git-common-dir")
}

// Current branch name of a checkout, nil when detached or outside a repo
func gitCurrentBranch(path string) *string {
	return gitOut(path, 30*time.Second, "branch", "--show-current")
}

// Reports whether the path is a linked worktree whose git dir differs from the common dir
func isLinkedWorktreeCheckout(path string) (bool, error) {
	out := gitOut(path, 30*time.Second, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir")
	var lines []string
	if out != nil {
		lines = pystr.Splitlines(*out, false)
	}
	if len(lines) != 2 {
		return false, nil
	}
	gitDir, err := pypath.Resolve(pypath.PathStr(lines[0]))
	if err != nil {
		return false, err
	}
	commonDir, err := pypath.Resolve(pypath.PathStr(lines[1]))
	if err != nil {
		return false, err
	}
	return gitDir != commonDir, nil
}

// Closest existing ancestor of a path, the path itself when it exists
func nearestExistingPath(path string) string {
	current := pypath.PathStr(path)
	for !pathExists(current) {
		parent := pypath.PathParent(current)
		if parent == current {
			break
		}
		current = parent
	}
	return current
}

// Git toplevel of the nearest existing ancestor of a worktree target, nil when none is a repo
func repoRootForWorktreeTarget(path string) (*string, error) {
	current, err := pypath.Resolve(nearestExistingPath(path))
	if err != nil {
		return nil, err
	}
	for {
		if root := gitToplevel(current); root != nil {
			return root, nil
		}
		parent := pypath.PathParent(current)
		if parent == current {
			return nil, nil
		}
		current = parent
	}
}

// Materializes target as a linked git worktree of repoRoot on branchName
func ensureGitWorktree(repoRoot, target, branchName string) error {
	target, err := pypath.PathExpanduser(target)
	if err != nil {
		return err
	}
	repoCommon, err := gitCommonDir(repoRoot)
	if err != nil {
		return err
	}
	if pathExists(target) && repoCommon != nil {
		targetCommon, err := gitCommonDir(target)
		if err != nil {
			return err
		}
		if targetCommon != nil && pathKey(*targetCommon) == pathKey(*repoCommon) {
			return nil
		}
	}
	if err := os.MkdirAll(pypath.PathParent(target), 0o777); err != nil {
		return err
	}
	args := []string{"worktree", "add", "-b", branchName, target, "HEAD"}
	if gitBranchExists(repoRoot, branchName) {
		args = []string{"worktree", "add", target, branchName}
	}
	result, err := wsGit(repoRoot, 60*time.Second, args...)
	if err != nil {
		return err
	}
	if result.Returncode != 0 {
		return &RuntimeError{fmt.Sprintf("git worktree add failed for %s on branch %s: %s", target, branchName, result.failureText())}
	}
	return nil
}

// Materializes the canonical repo .worktrees task worktree
func anchoredWorktree(repoRoot, taskID, branchName string) (string, string, error) {
	target := pypath.PathJoin(repoRoot, ".worktrees", taskID)
	if err := ensureGitWorktree(repoRoot, target, branchName); err != nil {
		return "", "", err
	}
	return target, branchName, nil
}

// Resolves and materializes a linked git worktree for the task, anchoring on the board default workdir when no path is set
func resolveWorktreeWorkspace(ctx context.Context, task *Task, board *string) (string, string, error) {
	branchName := ""
	if task.BranchName != nil {
		branchName = pystr.Strip(*task.BranchName)
	}
	if branchName == "" {
		branchName = "wt/" + task.ID
	}
	if task.WorkspacePath == nil || *task.WorkspacePath == "" {
		boardSlug := GetCurrentBoard(ctx)
		if board != nil && *board != "" {
			boardSlug = *board
		}
		meta, err := ReadBoardMetadata(&boardSlug)
		if err != nil {
			return "", "", err
		}
		boardDefault := ""
		if v, _ := meta.Get("default_workdir"); truthy(v) {
			boardDefault = pystr.Strip(cellStr(v))
		}
		if boardDefault == "" {
			return "", "", &ValueError{fmt.Sprintf("task %s has workspace_kind=worktree but no workspace_path, "+
				"and board %s has no default_workdir set. Set a board "+
				"default workdir (a git repo) or create the task with "+
				"--workspace worktree:<absolute-repo-path>.", task.ID, pyrepr.Repr(boardSlug))}
		}
		anchor, err := pypath.PathExpanduser(boardDefault)
		if err != nil {
			return "", "", err
		}
		if !pypath.IsAbs(anchor) {
			return "", "", &ValueError{fmt.Sprintf("board %s default_workdir %s is not "+
				"absolute; use an absolute path to a git repo", pyrepr.Repr(boardSlug), pyrepr.Repr(boardDefault))}
		}
		repoRoot := gitToplevel(anchor)
		if repoRoot == nil {
			return "", "", &ValueError{fmt.Sprintf("task %s has workspace_kind=worktree but board "+
				"%s default_workdir %s is not inside a git repo", task.ID, pyrepr.Repr(boardSlug), pyrepr.Repr(boardDefault))}
		}
		return anchoredWorktree(*repoRoot, task.ID, branchName)
	}
	requested, err := pypath.PathExpanduser(*task.WorkspacePath)
	if err != nil {
		return "", "", err
	}
	if !pypath.IsAbs(requested) {
		return "", "", &ValueError{fmt.Sprintf("task %s has non-absolute worktree path "+
			"%s; use an absolute path", task.ID, pyrepr.Repr(*task.WorkspacePath))}
	}
	requestedResolved, err := pypath.Resolve(requested)
	if err != nil {
		return "", "", err
	}
	if pathExists(requested) {
		linked, err := isLinkedWorktreeCheckout(requested)
		if err != nil {
			return "", "", err
		}
		if linked {
			actualBranch := gitCurrentBranch(requested)
			if actualBranch != nil && *actualBranch == branchName {
				return requestedResolved, *actualBranch, nil
			}
			fallbackRoot, err := repoRootForWorktreeTarget(pypath.PathParent(requested))
			if err != nil {
				return "", "", err
			}
			if fallbackRoot != nil {
				fallback := pypath.PathJoin(*fallbackRoot, ".worktrees", task.ID)
				fallbackResolved, err := pypath.Resolve(fallback)
				if err != nil {
					return "", "", err
				}
				if pathKey(fallbackResolved) != pathKey(requestedResolved) {
					if err := ensureGitWorktree(*fallbackRoot, fallback, branchName); err != nil {
						return "", "", err
					}
					return fallbackResolved, branchName, nil
				}
			}
			branch := branchName
			if actualBranch != nil && *actualBranch != "" {
				branch = *actualBranch
			}
			return requestedResolved, branch, nil
		}
	}
	if repoRoot := gitToplevel(requested); repoRoot != nil && pathKey(requestedResolved) == pathKey(*repoRoot) {
		return anchoredWorktree(*repoRoot, task.ID, branchName)
	}
	repoRoot, err := repoRootForWorktreeTarget(pypath.PathParent(requested))
	if err != nil {
		return "", "", err
	}
	if repoRoot == nil {
		return "", "", &ValueError{fmt.Sprintf("task %s worktree path %s is not inside a git repo "+
			"and does not point at a git repo root", task.ID, pyrepr.Repr(*task.WorkspacePath))}
	}
	if err := ensureGitWorktree(*repoRoot, requested, branchName); err != nil {
		return "", "", err
	}
	return requested, branchName, nil
}

// Resolves and creates the workspace directory of a task by its kind
func ResolveWorkspace(ctx context.Context, task *Task, board *string) (string, error) {
	kind := task.WorkspaceKind
	if kind == "" {
		kind = "scratch"
	}
	hasPath := task.WorkspacePath != nil && *task.WorkspacePath != ""
	var p string
	switch {
	case kind == "worktree":
		p, _, err := resolveWorktreeWorkspace(ctx, task, board)
		return p, err
	case kind == "scratch" && !hasPath:
		root, err := WorkspacesRoot(ctx, board)
		if err != nil {
			return "", err
		}
		p = pypath.PathJoin(root, task.ID)
	case kind == "scratch":
		expanded, err := pypath.PathExpanduser(*task.WorkspacePath)
		if err != nil {
			return "", err
		}
		if !pypath.IsAbs(expanded) {
			return "", &ValueError{fmt.Sprintf("task %s has non-absolute workspace_path "+
				"%s; workspace paths must be absolute", task.ID, pyrepr.Repr(*task.WorkspacePath))}
		}
		p = expanded
	case kind == "dir":
		if !hasPath {
			return "", &ValueError{fmt.Sprintf("task %s has workspace_kind=dir but no workspace_path", task.ID)}
		}
		expanded, err := pypath.PathExpanduser(*task.WorkspacePath)
		if err != nil {
			return "", err
		}
		if !pypath.IsAbs(expanded) {
			return "", &ValueError{fmt.Sprintf("task %s has non-absolute workspace_path "+
				"%s; use an absolute path "+
				"(relative paths are ambiguous against the dispatcher's CWD)", task.ID, pyrepr.Repr(*task.WorkspacePath))}
		}
		p = expanded
	default:
		return "", &ValueError{fmt.Sprintf("unknown workspace_kind: %s", kind)}
	}
	if err := os.MkdirAll(p, 0o777); err != nil {
		return "", err
	}
	return p, nil
}

// Writes one task column inside its own write transaction
func setTaskColumn(ctx context.Context, conn state.SQLiteConn, taskID, column, value string) error {
	return WriteTxn(ctx, conn, false, func() error {
		_, err := conn.ExecContext(ctx, "UPDATE tasks SET "+column+" = ? WHERE id = ?", value, taskID)
		return err
	})
}

// Persists the resolved workspace path of a task
func SetWorkspacePath(ctx context.Context, conn state.SQLiteConn, taskID, path string) error {
	return setTaskColumn(ctx, conn, taskID, "workspace_path", path)
}

// Persists the worktree branch name of a task
func SetBranchName(ctx context.Context, conn state.SQLiteConn, taskID, branchName string) error {
	return setTaskColumn(ctx, conn, taskID, "branch_name", branchName)
}
