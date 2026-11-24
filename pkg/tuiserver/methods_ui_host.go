package tuiserver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/tools"
)

var uiHostRegistry = &handlerRegistry{}

func init() {
	cwd := []requiredParam{{"cwd", strippedPresent}}
	guardedRPC(uiHostRegistry, "snapshot.list", 5000, rpcShape{}, snapshotList)
	guardedRPC(uiHostRegistry, "snapshot.create", 5000, rpcShape{}, snapshotCreate)
	guardedRPC(uiHostRegistry, "snapshot.prune", 5000, rpcShape{}, snapshotPrune)
	guardedRPC(uiHostRegistry, "snapshot.target", 5000, rpcShape{}, snapshotTarget)
	guardedRPC(uiHostRegistry, "profiles.export", 5000, rpcShape{}, profilesExport)
	guardedRPC(uiHostRegistry, "profiles.import", 5000, rpcShape{required: []requiredParam{{"archive", strippedPresent}}}, profilesImport)
	guardedRPC(uiHostRegistry, "tools.list", 5000, rpcShape{}, toolsList)
	guardedRPC(uiHostRegistry, "worktree.status", 5000, rpcShape{required: cwd}, worktreeStatus)
	guardedRPC(uiHostRegistry, "worktree.list", 5000, rpcShape{required: cwd}, worktreeList)
	guardedRPC(uiHostRegistry, "worktree.prune", 5000, rpcShape{required: cwd}, worktreePrune)
	guardedRPC(uiHostRegistry, "worktree.create", 5000, rpcShape{required: cwd}, worktreeCreate)
	guardedRPC(uiHostRegistry, "worktree.record", 5000, rpcShape{required: []requiredParam{{"path", strippedPresent}}}, worktreeRecord)
	guardedRPC(uiHostRegistry, "worktree.settle", 5000, rpcShape{required: []requiredParam{{"path", strippedPresent}}}, worktreeSettle)
	uiHostRegistry.install()
}

// Shared command catalog line of the profile's language
func commandLine(ctx context.Context, key string, kwargs map[string]any) string {
	return i18n.T(ctx, "cli.commands."+key, "", kwargs)
}

// Lines of the shared /snapshot subcommands
type SnapshotLines interface {
	List() ([]string, error)
	Create(parts []string) ([]string, error)
	Prune(parts []string) ([]string, error)
	RestoreTarget(parts []string) (string, []string, error)
}

// Shared /snapshot command over a profile home and a catalog lookup, set by the wiring
var QuickSnapshots func(home string, t func(key string, kwargs map[string]any) string) SnapshotLines

// Snapshot command over the scoped profile home
func snapshotCommand(ctx context.Context) SnapshotLines {
	return QuickSnapshots(home.Dir(ctx), func(key string, kwargs map[string]any) string { return commandLine(ctx, key, kwargs) })
}

// Words of a string list param, other entries skipped
func wordsParam(v any) []string {
	out := []string{}
	items, _ := v.([]any)
	for _, item := range items {
		if word, isStr := item.(string); isStr {
			out = append(out, word)
		}
	}
	return out
}

// Snapshot subcommand words with the two leading words the shared command expects
func snapshotParts(sub string, params map[string]any) []string {
	return append([]string{"/snapshot", sub}, wordsParam(params["args"])...)
}

// Lines answer, an empty list for none
func linesResult(rid any, lines []string) map[string]any {
	if lines == nil {
		lines = []string{}
	}
	return ok(rid, map[string]any{"lines": lines})
}

func snapshotList(ctx context.Context, rid any, _ map[string]any) (map[string]any, error) {
	lines, err := snapshotCommand(ctx).List()
	if err != nil {
		return nil, err
	}
	return linesResult(rid, lines), nil
}

func snapshotCreate(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	lines, err := snapshotCommand(ctx).Create(snapshotParts("create", params))
	if err != nil {
		return nil, err
	}
	return linesResult(rid, lines), nil
}

func snapshotPrune(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	lines, err := snapshotCommand(ctx).Prune(snapshotParts("prune", params))
	if err != nil {
		return nil, err
	}
	return linesResult(rid, lines), nil
}

func snapshotTarget(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	id, lines, err := snapshotCommand(ctx).RestoreTarget(snapshotParts("restore", params))
	if err != nil {
		return nil, err
	}
	var target any
	if lines == nil {
		target, lines = id, []string{}
	}
	return ok(rid, map[string]any{"id": target, "lines": lines}), nil
}

// Archive of the named or scoped profile at an absolute output path or in the export directory
func profilesExport(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	name := strArg(params, "name")
	if name == "" {
		name = profiles.GetActiveProfileName(ctx)
	}
	if name == "" {
		name = "default"
	}
	target := strArg(params, "output")
	if target == "" {
		path, err := manage.GetProfileExportPath(name, "")
		if err != nil {
			return nil, err
		}
		target = path
	}
	written, err := manage.ExportProfile(name, target, nil)
	if err != nil {
		return nil, err
	}
	return ok(rid, map[string]any{"name": name, "path": written}), nil
}

// New profile from an archive with its wrapper alias unless the name collides with a command
func profilesImport(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	profileDir, err := manage.ImportProfile(strArg(params, "archive"), strArg(params, "name"))
	if err != nil {
		return nil, err
	}
	imported := filepath.Base(profileDir)
	var wrapper any
	if collision, err := profiles.CheckAliasCollision(imported); err == nil && collision == "" {
		if script, err := profiles.CreateWrapperScript(imported, ""); err == nil && script != "" {
			wrapper = script
		}
	}
	return ok(rid, map[string]any{"name": imported, "path": profileDir, "wrapper": wrapper}), nil
}

// Capturing surface of the shared per toolset listing, plain text and never interactive
type toolsListUI struct{ buf bytes.Buffer }

func (u *toolsListUI) Out() io.Writer                        { return &u.buf }
func (u *toolsListUI) Color(text string, _ ...string) string { return text }
func (u *toolsListUI) Prompt(_, def string, _ bool) string   { return def }
func (u *toolsListUI) YesNo(_ string, def bool) bool         { return def }
func (u *toolsListUI) Checklist(_ string, _ []string, picked, _ map[int]bool) map[int]bool {
	return picked
}
func (u *toolsListUI) SingleSelect(string, []string) (int, bool) { return 0, false }
func (u *toolsListUI) Interactive() bool                         { return false }

// Lines of text without escapes, trailing blank lines dropped
func plainLines(text string) []string {
	text = strings.TrimRight(tools.StripANSI(text), "\n")
	if text == "" {
		return []string{}
	}
	return strings.Split(text, "\n")
}

func toolsList(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	platform := strArg(params, "platform")
	if platform == "" {
		platform = "cli"
	}
	ui := &toolsListUI{}
	if err := mcp.ToolsDisableEnableCommand(ctx, ui, "list", platform, nil); err != nil {
		return nil, err
	}
	return linesResult(rid, plainLines(ui.buf.String())), nil
}

// Repository root of a folder, nil outside a repository
func repoRootOrNil(root string) any {
	if root == "" {
		return nil
	}
	return root
}

// Stdout of a git command in a folder within ten seconds, false when it failed
func gitOutput(ctx context.Context, dir string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.ToValidUTF8(string(out), "\ufffd"), true
}

// Tree the process locked with its -w launch in the launch folder's repository, nil when it made none
func launchWorktree(ctx context.Context, cwd string, pid int) any {
	if cwd == "" || pid <= 0 {
		return nil
	}
	text, done := gitOutput(ctx, cwd, "worktree", "list", "--porcelain")
	if !done {
		return nil
	}
	owner := fmt.Sprintf("icebath pid=%d", pid)
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
		return map[string]any{"path": entry["worktree"], "branch": strings.TrimPrefix(entry["branch"], "refs/heads/")}
	}
	return nil
}

func worktreeStatus(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	return ok(rid, map[string]any{"repo_root": repoRootOrNil(tools.GitRepoRootOf(strArg(params, "cwd"))),
		"launch": launchWorktree(ctx, strArg(params, "launch_cwd"), intParam(params, "pid", 0))}), nil
}

func worktreeList(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	root := tools.GitRepoRootOf(strArg(params, "cwd"))
	lines := []string{}
	if root != "" {
		if text, done := gitOutput(ctx, root, "worktree", "list"); done && strings.TrimSpace(text) != "" {
			for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
				lines = append(lines, strings.TrimRight(line, "\r"))
			}
		}
	}
	return ok(rid, map[string]any{"repo_root": repoRootOrNil(root), "lines": lines}), nil
}

// Reclaims stale trees and merged branches but the kept path, and names the trees kept for a reason
func worktreePrune(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	root := tools.GitRepoRootOf(strArg(params, "cwd"))
	out := map[string]any{"repo_root": repoRootOrNil(root), "actions": []string{}, "preserved": []any{}, "error": nil}
	if root == "" {
		return ok(rid, out), nil
	}
	dryRun, keep := params["dry_run"] == true, strArg(params, "keep")
	records, err := tools.AuditWorktrees(ctx, root, false, nil)
	if err != nil {
		out["error"] = err.Error()
		return ok(rid, out), nil
	}
	kept := []tools.TreeRecord{}
	for _, r := range records {
		if keep == "" || r.Path != keep {
			kept = append(kept, r)
		}
	}
	actions, err := tools.ReclaimWorktrees(ctx, root, dryRun, kept)
	if err == nil {
		var branchActions []string
		branchActions, err = tools.ReclaimBranches(root, dryRun, nil)
		actions = append(actions, branchActions...)
	}
	if err != nil {
		out["error"] = err.Error()
		return ok(rid, out), nil
	}
	preserved := []any{}
	for _, r := range kept {
		if r.Verdict == "keep" && !strings.Contains(r.Reason, "kanban") && !strings.Contains(r.Reason, "in use") {
			preserved = append(preserved, map[string]any{"name": r.Name, "reason": r.Reason})
		}
	}
	if actions == nil {
		actions = []string{}
	}
	out["actions"], out["preserved"] = actions, preserved
	return ok(rid, out), nil
}

// Whether the profile syncs a new tree with its upstream base, on unless worktree_sync says otherwise
func worktreeSyncBase(ctx context.Context) bool {
	h := home.Dir(ctx)
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return true
	}
	if v, set := cfg["worktree_sync"]; set {
		return truthy(v)
	}
	return true
}

// Tree on a new branch of the folder's repository, locked for the asking process, with its creation report
func worktreeCreate(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	root := tools.GitRepoRootOf(strArg(params, "cwd"))
	out := map[string]any{"repo_root": repoRootOrNil(root), "tree": nil, "lines": []string{}}
	if root == "" {
		return ok(rid, out), nil
	}
	var report bytes.Buffer
	info := tools.SetupWorktreeFor(ctx, root, worktreeSyncBase(ctx), strArg(params, "name"), nil, intParam(params, "pid", 0), &report)
	out["lines"] = plainLines(report.String())
	if len(info) > 0 {
		out["tree"] = map[string]any{"path": info["path"], "branch": info["branch"], "repo_root": info["repo_root"]}
	}
	return ok(rid, out), nil
}

// Tree record of the path, branch and repository params
func treeParams(params map[string]any) map[string]string {
	return map[string]string{"path": strArg(params, "path"), "branch": strArg(params, "branch"), "repo_root": strArg(params, "repo_root")}
}

func worktreeRecord(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	tools.RecordTUIWorktree(ctx, treeParams(params))
	return ok(rid, map[string]any{"ok": true}), nil
}

func worktreeSettle(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	var report bytes.Buffer
	SettleWorktree(ctx, treeParams(params), "", func(text string) { fmt.Fprintln(&report, text) })
	return linesResult(rid, plainLines(strings.TrimLeft(report.String(), "\n"))), nil
}

// Carries out an exit choice, else removes a clean tree and its branch or echoes why the tree is kept
func SettleWorktree(ctx context.Context, tree map[string]string, choice string, echo func(string)) {
	path, branch, repoRoot := tree["path"], tree["branch"], tree["repo_root"]
	if _, err := os.Stat(path); err != nil {
		return
	}
	info := map[string]any{"path": path, "branch": branch, "repo_root": repoRoot}
	if choice != "" {
		info["exit_choice"] = choice
	}
	if tools.ApplyExitChoice(ctx, info, echo) {
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
