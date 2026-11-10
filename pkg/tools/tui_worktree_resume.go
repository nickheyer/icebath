package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	tuiWorktreeRecordName = "tui_worktrees.json"
	tuiWorktreeMaxRecords = 500
)

// Reentrant holder of the TUI worktree record lock
var tuiWorktreeLockHolder = &fileLockHolder{}

func tuiWorktreeRecordPath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), tuiWorktreeRecordName)
}

// Case normalized real path of a tree, the key of its record
func tuiWorktreeKey(path string) string {
	real, err := pypath.Realpath(path, pypath.Lax)
	if err != nil {
		real = filepath.Clean(path)
	}
	return pypath.Normcase(real)
}

// Recorded trees in file order, empty when the record is missing, malformed or holds no object
func readTUIWorktrees(ctx context.Context) *pyjson.Object {
	raw, err := os.ReadFile(tuiWorktreeRecordPath(ctx))
	if err != nil {
		return pyjson.NewObject()
	}
	parsed, err := pyjson.Loads(pystr.Lstrip(string(raw)))
	if err != nil {
		return pyjson.NewObject()
	}
	doc, ok := parsed.(*pyjson.Object)
	if !ok {
		return pyjson.NewObject()
	}
	trees, ok := doc.Vals["trees"].(*pyjson.Object)
	if !ok {
		return pyjson.NewObject()
	}
	return trees
}

// Tree record fields as strings, empty for an absent or null value
func tuiRecordField(entry *pyjson.Object, key string) string {
	v, _ := entry.Get(key)
	if v == nil {
		return ""
	}
	return pyrepr.Str(v)
}

// Remembers a tree the TUI worktree launcher created or a worktree new moved a session into, never raising
func RecordTUIWorktree(ctx context.Context, wtInfo map[string]string) {
	log := toolsLogger(nil)
	path := tuiWorktreeRecordPath(ctx)
	release, err := acquireFileLock(path+".lock", tuiWorktreeLockHolder, 5*time.Second, "Timed out waiting for the TUI worktree record lock")
	if err != nil {
		log.Debug(fmt.Sprintf("TUI worktree record failed for %s: %s", wtInfo["path"], err))
		return
	}
	defer release()
	kept := pyjson.NewObject()
	trees := readTUIWorktrees(ctx)
	for _, k := range trees.Keys {
		entry, ok := trees.Vals[k].(*pyjson.Object)
		if !ok {
			continue
		}
		if _, err := os.Stat(filepath.Join(tuiRecordField(entry, "repo_root"), ".git")); err == nil {
			kept.Set(k, entry)
		}
	}
	record := pyjson.NewObject()
	record.Set("path", wtInfo["path"])
	record.Set("branch", wtInfo["branch"])
	record.Set("repo_root", wtInfo["repo_root"])
	kept.Set(tuiWorktreeKey(wtInfo["path"]), record)
	if len(kept.Keys) > tuiWorktreeMaxRecords {
		trimmed := pyjson.NewObject()
		for _, k := range kept.Keys[len(kept.Keys)-tuiWorktreeMaxRecords:] {
			trimmed.Set(k, kept.Vals[k])
		}
		kept = trimmed
	}
	doc := pyjson.NewObject()
	doc.Set("version", 1)
	doc.Set("trees", kept)
	if err := config.AtomicJSONWrite(ctx, path, doc); err != nil {
		log.Debug(fmt.Sprintf("TUI worktree record failed for %s: %s", wtInfo["path"], err))
	}
}

// The launcher's record of the missing tree cwd is or sits inside, nil when none was recorded there
func recordedTreeFor(ctx context.Context, cwd string) map[string]string {
	trees := readTUIWorktrees(ctx)
	probe := filepath.Clean(cwd)
	for candidate := probe; ; candidate = filepath.Dir(candidate) {
		entry, ok := trees.Vals[tuiWorktreeKey(candidate)].(*pyjson.Object)
		if ok {
			repo, tree, branch := tuiRecordField(entry, "repo_root"), tuiRecordField(entry, "path"), tuiRecordField(entry, "branch")
			if _, err := os.Stat(filepath.Join(repo, ".git")); branch != "" && err == nil &&
				tuiWorktreeKey(filepath.Dir(tree)) == tuiWorktreeKey(filepath.Join(repo, ".worktrees")) && tuiWorktreeKey(tree) == tuiWorktreeKey(candidate) {
				return map[string]string{"path": tree, "branch": branch, "repo_root": repo}
			}
			return nil
		}
		if filepath.Dir(candidate) == candidate {
			return nil
		}
	}
}

// Whether the branch exists in the repository
func tuiBranchExists(repoRoot, branch string) bool {
	result, err := worktreeGit([]string{"rev-parse", "--verify", "--quiet", "refs/heads/" + branch}, repoRoot, 0, nil)
	return err == nil && result.Returncode == 0
}

// RecreateTUIWorktree brings back the launcher made tree a resumed session's missing cwd names, empty when it cannot
func RecreateTUIWorktree(ctx context.Context, cwd, sessionID string) string {
	if cwd == "" {
		return ""
	}
	if st, err := os.Stat(cwd); err == nil && st.IsDir() {
		return ""
	}
	entry := recordedTreeFor(ctx, cwd)
	if entry == nil {
		return ""
	}
	repoRoot, tree, branch := entry["repo_root"], entry["path"], entry["branch"]
	if _, err := os.Lstat(tree); err == nil {
		return ""
	}
	env := NoninteractiveRepoGitEnv(repoRoot, nil)
	if env == nil {
		return ""
	}
	log := toolsLogger(nil)
	worktreeGitQuiet([]string{"worktree", "prune"}, repoRoot, 15*time.Second, "", env)
	if err := os.MkdirAll(filepath.Dir(tree), 0o777); err != nil {
		log.Warn(fmt.Sprintf("could not recreate worktree %s: %s", tree, pypath.OSErrorText(err)))
		return ""
	}
	args := []string{"worktree", "add", tree, "-b", branch, "HEAD"}
	if tuiBranchExists(repoRoot, branch) {
		args = []string{"worktree", "add", tree, branch}
	}
	result, err := worktreeGit(args, repoRoot, 120*time.Second, env)
	if err != nil {
		log.Warn(fmt.Sprintf("could not recreate worktree %s: %s", tree, err))
		return ""
	}
	if result.Returncode != 0 {
		log.Warn(fmt.Sprintf("could not recreate worktree %s: %s", tree, pystr.Strip(result.Stderr)))
		return ""
	}
	copyWorktreeIncludes(repoRoot, tree)
	retainWorktreeForSession(repoRoot, tree, sessionID, home.StatePath(home.Dir(ctx)))
	return tree
}
