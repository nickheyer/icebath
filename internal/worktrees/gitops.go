package worktrees

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/git"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Runs git in the folder with the environment, the process's own when nil
func run(dir string, env map[string]string, timeout time.Duration, args ...string) (*git.Result, error) {
	return git.Run(context.Background(), dir, env, timeout, nil, args...)
}

// Trimmed output of git in the folder and whether it exited zero; an error when it could not run
func output(dir string, timeout time.Duration, args ...string) (string, bool, error) {
	result, err := run(dir, nil, timeout, args...)
	if err != nil {
		return "", false, err
	}
	if result.Code != 0 {
		return "", false, nil
	}
	return strings.TrimSpace(result.Stdout), true, nil
}

// Runs git, any failure logged at debug under the note
func quiet(dir string, env map[string]string, timeout time.Duration, note string, args ...string) {
	if _, err := run(dir, env, timeout, args...); err != nil && note != "" {
		slog.Debug(note, "error", err)
	}
}

// Environment of git runs here that never prompt, page or read the user's config
func plain() map[string]string { return git.Env(context.Background(), nil) }

// Entries of the repository's .worktreeinclude, the files copied and folders linked into every tree made of it
func includes(repo string) []string {
	path := filepath.Join(repo, ".worktreeinclude")
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []string
	for line := range strings.Lines(strings.TrimPrefix(strings.ToValidUTF8(string(data), "�"), bom)) {
		if entry := strings.TrimSpace(line); entry != "" && !strings.HasPrefix(entry, "#") {
			entries = append(entries, entry)
		}
	}
	return entries
}

// Byte order mark a text file may start with
const bom = "\xef\xbb\xbf"

// Path with its links resolved, absolute
func canonical(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// Whether the path is the folder or lies under it
func under(path, folder string) bool {
	rel, err := filepath.Rel(folder, path)
	return err == nil && filepath.IsLocal(rel)
}

// Copies the files and links the folders .worktreeinclude names into the tree, an entry leaving the repository or the tree skipped
func copyIncludes(repo, tree string) {
	entries := includes(repo)
	if len(entries) == 0 {
		return
	}
	repoReal, err1 := canonical(repo)
	treeReal, err2 := canonical(tree)
	if err := errors.Join(err1, err2); err != nil {
		slog.Debug("Error copying .worktreeinclude entries", "error", err)
		return
	}
	for _, entry := range entries {
		src, dst := filepath.Join(repo, entry), filepath.Join(tree, entry)
		srcReal, err1 := canonical(src)
		dstReal, err2 := canonical(dst)
		if err1 != nil || err2 != nil {
			slog.Debug("Skipping invalid .worktreeinclude entry: " + entry)
			continue
		}
		if !under(srcReal, repoReal) {
			slog.Warn("Skipping .worktreeinclude entry outside repo root: " + entry)
			continue
		}
		if !under(dstReal, treeReal) {
			slog.Warn("Skipping .worktreeinclude entry that escapes worktree: " + entry)
			continue
		}
		info, err := os.Stat(src)
		if err != nil {
			continue
		}
		if info.Mode().IsRegular() {
			if err := os.MkdirAll(filepath.Dir(dst), 0o777); err == nil {
				err = copyFile(src, dst)
			}
			if err != nil {
				slog.Debug("Error copying .worktreeinclude entries", "error", err)
				return
			}
			continue
		}
		if _, err := os.Lstat(dst); !info.IsDir() || err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
			slog.Debug("Error copying .worktreeinclude entries", "error", err)
			return
		}
		if err := os.Symlink(srcReal, dst); err != nil {
			if runtime.GOOS != "windows" {
				slog.Debug("Error copying .worktreeinclude entries", "error", err)
				return
			}
			slog.Info(".worktreeinclude: symlink failed — falling back to a copy on Windows.", "error", err)
			if err := copyTree(srcReal, dst); err != nil {
				slog.Warn(".worktreeinclude: copy fallback also failed", "from", src, "to", dst, "error", err)
			}
		}
	}
}

// Copies a file with its permissions
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Copies a folder and what it holds, links copied as links, into a folder that may be there already
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.IsDir():
			return os.MkdirAll(target, 0o777)
		}
		return copyFile(path, target)
	})
}

// Entries of .worktreeinclude the tree holds as links back to the same entry of the main checkout, untracked yet no work
func includeLinks(tree, repo string) map[string]bool {
	links := map[string]bool{}
	for _, entry := range includes(repo) {
		dst := filepath.Join(tree, entry)
		if info, err := os.Lstat(dst); err != nil || info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		a, errA := filepath.EvalSymlinks(dst)
		b, errB := filepath.EvalSymlinks(filepath.Join(repo, entry))
		if errA == nil && errB == nil && a == b {
			links[strings.TrimRight(filepath.ToSlash(filepath.Clean(entry)), "/")] = true
		}
	}
	return links
}

// Trunk of a repository without remote branches: main or master, else the branch of its main checkout; empty when none
func localTrunk(path string, timeout time.Duration) (string, error) {
	for _, name := range []string{"main", "master"} {
		out, ok, err := output(path, timeout, "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
		if err != nil {
			return "", err
		}
		if ok && out != "" {
			return name, nil
		}
	}
	listing, _, err := output(path, timeout, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	first, _, _ := strings.Cut(listing, "\n\n")
	for line := range strings.Lines(first) {
		if branch, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "branch refs/heads/"); ok {
			return strings.TrimSpace(branch), nil
		}
	}
	return "", nil
}

// Ref merged work is judged against: the trunk of origin, or the local trunk of a repository with no remote branches; empty when nothing
// compares
func mergeBase(path string, timeout time.Duration) (string, error) {
	for _, candidate := range []string{"origin/HEAD", "origin/main", "origin/master"} {
		out, ok, err := output(path, timeout, "rev-parse", "--verify", "--quiet", candidate)
		if err != nil {
			return "", err
		}
		if ok && out != "" {
			return candidate, nil
		}
	}
	remotes, ok, err := output(path, timeout, "for-each-ref", "--format=%(refname)", "refs/remotes")
	if err != nil {
		return "", err
	}
	if ok && remotes == "" {
		return localTrunk(path, timeout)
	}
	return "", nil
}

// Whether the tree's branch holds commits no remote branch has, or no trunk when there are no remotes; true when git can't tell
func Unpushed(tree string, timeout time.Duration) bool {
	remotes, ok, err := output(tree, timeout, "for-each-ref", "--format=%(refname)", "refs/remotes")
	if err != nil || !ok {
		return true
	}
	against := []string{"--remotes"}
	if remotes == "" {
		trunk, err := localTrunk(tree, timeout)
		if err != nil || trunk == "" {
			return true
		}
		against = []string{trunk}
	}
	commits, ok, err := output(tree, timeout, append([]string{"log", "--oneline", "HEAD", "--not"}, against...)...)
	return err != nil || !ok || commits != ""
}

// Whether the tree holds staged, unstaged or untracked changes, or edits git was told to overlook; true when git can't tell. Links back to
// the main checkout that .worktreeinclude made are no change
func Dirty(tree, repo string, timeout time.Duration) bool {
	env := repoEnv(tree)
	if env == nil {
		return true
	}
	result, err := run(tree, env, timeout, "status", "--porcelain", "-z", "--untracked-files=normal")
	if err != nil || result.Code != 0 {
		return true
	}
	var entries []string
	for _, entry := range strings.Split(result.Stdout, "\x00") {
		if entry != "" {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		edits, err := hiddenEdits(tree, nil)
		return err != nil || len(edits) > 0
	}
	links := includeLinks(tree, repo)
	for _, entry := range entries {
		if !strings.HasPrefix(entry, "?? ") || !links[strings.TrimRight(entry[3:], "/")] {
			return true
		}
	}
	return false
}

// Whether the repository is a shallow clone, whose push state can't be told; false when git can't tell
func Shallow(repo string, timeout time.Duration) bool {
	out, ok, err := output(repo, timeout, "rev-parse", "--is-shallow-repository")
	return err == nil && ok && out == "true"
}

// Fetches the whole history of a shallow clone without its file contents, or with them when the remote refuses filters, so whether a
// tree's commits were pushed can be told: whether the clone is shallow no more
func deepen(repo string, timeout time.Duration) bool {
	if !Shallow(repo, 5*time.Second) {
		return true
	}
	remotes, ok, err := output(repo, 10*time.Second, "remote")
	if err != nil {
		slog.Debug("Deepening shallow repo failed (non-fatal)", "error", err)
		return false
	}
	names := git.Lines(remotes)
	if !ok || len(names) == 0 {
		return false
	}
	remote := names[0]
	if slices.Contains(names, "origin") {
		remote = "origin"
	}
	defer git.MarkPromisor(repo)
	for _, extra := range [][]string{{"--filter=blob:none"}, nil} {
		result, err := run(repo, plain(), timeout, append([]string{"fetch", remote, "--unshallow"}, extra...)...)
		if err != nil {
			slog.Debug("Deepening shallow repo failed (non-fatal)", "error", err)
			return false
		}
		if result.Code == 0 {
			break
		}
		stderr := strings.TrimSpace(result.Stderr)
		slog.Debug(strings.TrimSpace("git fetch --unshallow "+strings.Join(extra, " ")) + " failed: " + stderr[max(0, len(stderr)-500):])
	}
	deepened := !Shallow(repo, 5*time.Second)
	if deepened {
		slog.Info("Deepened shallow clone so worktree cleanup can verify push state", "repo", repo)
	}
	return deepened
}

// Verdicts of whether a branch's commits are all upstream, kept on disk between runs, in the order they were reached
type verdicts struct {
	mu   sync.Mutex
	path string
	keys []string
	vals map[string]bool
	// Count of verdicts when read, so an unchanged cache is not written again
	read int
}

// Verdicts kept on disk
const verdictsMax = 1000

func (v *verdicts) get(key string) (bool, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	verdict, ok := v.vals[key]
	return verdict, ok
}

func (v *verdicts) set(key string, verdict bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.vals[key]; !ok {
		v.keys = append(v.keys, key)
	}
	v.vals[key] = verdict
}

// One verdict as the cache file keeps it
type verdict struct {
	Key    string `json:"key"`
	Merged bool   `json:"merged"`
}

// Verdicts the profile keeps, none when the file is missing or broken
func loadVerdicts(cfg *config.Config) *verdicts {
	v := &verdicts{path: filepath.Join(profileDir(cfg), "cache", "worktree_merge_verdicts.json"), vals: map[string]bool{}}
	var saved struct {
		Verdicts []verdict `json:"verdicts"`
	}
	if data, err := os.ReadFile(v.path); err == nil && json.Unmarshal(data, &saved) == nil {
		for _, entry := range saved.Verdicts {
			v.set(entry.Key, entry.Merged)
		}
	}
	v.read = len(v.keys)
	return v
}

// Keeps the newest verdicts when there are new ones, never failing the caller
func (v *verdicts) save() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.keys) == v.read {
		return
	}
	saved := struct {
		Version  int       `json:"version"`
		Verdicts []verdict `json:"verdicts"`
	}{Version: 1}
	for _, key := range v.keys[max(0, len(v.keys)-verdictsMax):] {
		saved.Verdicts = append(saved.Verdicts, verdict{key, v.vals[key]})
	}
	data, err := json.MarshalIndent(saved, "", " ")
	if err == nil {
		err = fsx.Write(v.path, data, 0o600)
	}
	if err != nil {
		slog.Debug("Could not persist worktree merge cache", "error", err)
	}
}

// Whether every commit of the tree's branch not upstream is the same change as one that is, judged by git cherry for branches at most
// the given count ahead; the verdict kept by the commits it was made for
func allMerged(tree string, timeout time.Duration, ahead int, cache *verdicts) bool {
	base, err := mergeBase(tree, timeout)
	if err != nil || base == "" {
		return false
	}
	key := ""
	if cache != nil {
		revs, _, err := output(tree, timeout, "rev-parse", base+"^{commit}", "HEAD^{commit}")
		if err != nil {
			return false
		}
		if shas := strings.Fields(revs); len(shas) == 2 {
			key = fmt.Sprintf("%s..%s:%d", shas[0], shas[1], ahead)
			if merged, ok := cache.get(key); ok {
				return merged
			}
		}
	}
	keep := func(merged bool) bool {
		if cache != nil && key != "" {
			cache.set(key, merged)
		}
		return merged
	}
	counted, ok, err := output(tree, timeout, "rev-list", "--count", base+"..HEAD")
	if err != nil || !ok {
		return false
	}
	n, err := strconv.Atoi(cmp.Or(counted, "0"))
	switch {
	case err != nil:
		return false
	case n == 0:
		return keep(true)
	case n > ahead:
		return keep(false)
	}
	cherry, err := run(tree, nil, timeout, "cherry", base, "HEAD")
	if err != nil || cherry.Code != 0 {
		return false
	}
	lines := git.Lines(cherry.Stdout)
	return keep(len(lines) > 0 && !slices.ContainsFunc(lines, func(line string) bool { return !strings.HasPrefix(line, "-") }))
}

// Branch the tree has checked out, empty when it is detached or git fails
func currentBranch(tree string, timeout time.Duration) string {
	branch, ok, err := output(tree, timeout, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || !ok || branch == "HEAD" {
		return ""
	}
	return branch
}

// Whether the pull request of the tree's branch was merged on GitHub, asked of the gh command; only a merged verdict is kept
func prMerged(tree string, timeout time.Duration, cache *verdicts) bool {
	branch := currentBranch(tree, timeout)
	if branch == "" {
		return false
	}
	key := ""
	if cache != nil {
		sha, _, err := output(tree, timeout, "rev-parse", "HEAD")
		if err != nil {
			return false
		}
		if sha != "" {
			key = "pr-merged:" + branch + ":" + sha
			if merged, ok := cache.get(key); ok && merged {
				return true
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "list", "--head", branch, "--state", "merged", "--json", "number", "--limit", "1")
	cmd.Dir = tree
	cmd.Env = git.Environ(plain())
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	var prs []any
	if text := strings.TrimSpace(string(out)); text != "" && json.Unmarshal([]byte(text), &prs) != nil {
		return false
	}
	merged := len(prs) > 0
	if merged && cache != nil && key != "" {
		cache.set(key, true)
	}
	return merged
}

// Every branch of origin with the commit it is at, from one ls-remote; nil when origin can't be read
func remoteHeads(repo string, timeout time.Duration) map[string]string {
	result, err := run(repo, plain(), timeout, "ls-remote", "--heads", "origin")
	if err != nil || result.Code != 0 {
		return nil
	}
	heads := map[string]string{}
	for line := range strings.Lines(result.Stdout) {
		sha, name, ok := strings.Cut(strings.TrimRight(line, "\r\n"), "\t")
		if branch, isHead := strings.CutPrefix(name, "refs/heads/"); ok && isHead {
			heads[strings.TrimSpace(branch)] = strings.TrimSpace(sha)
		}
	}
	return heads
}

// Whether the tree's branch is exactly where origin has it
func pushedExact(tree string, heads map[string]string, timeout time.Duration) bool {
	branch := currentBranch(tree, timeout)
	if len(heads) == 0 || branch == "" || heads[branch] == "" {
		return false
	}
	head, ok, err := output(tree, timeout, "rev-parse", "HEAD")
	return err == nil && ok && head == heads[branch]
}

// Environment of git runs in one repository that also turns off its filter drivers, nil when they can't be found with certainty
func repoEnv(dir string) map[string]string { return git.RepoEnv(context.Background(), dir) }
