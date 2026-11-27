package worktrees

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/git"
)

// A tree made for a session to work in
type Tree struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Repo   string `json:"repo_root"`
	// What it was branched from
	Base string `json:"-"`
}

// Sweeps what a failed worktree add left: the partial folder, its locked admin entry and its branch
func cleanupFailedAdd(repo, path, branch string) {
	quiet(repo, nil, 15*time.Second, "", "worktree", "unlock", path)
	quiet(repo, nil, 15*time.Second, "", "worktree", "remove", "--force", path)
	if _, err := os.Lstat(path); err == nil {
		if err := os.RemoveAll(path); err != nil {
			slog.Debug("cleanup after failed worktree add", "error", err)
		}
	}
	quiet(repo, plain(), 15*time.Second, "", "worktree", "prune")
	quiet(repo, plain(), 15*time.Second, "", "branch", "-D", branch)
}

// Refspec that always writes the remote tracking ref, whatever the remote's fetch config says
func trackingRefspec(remote, branch string) string {
	return "+refs/heads/" + branch + ":refs/remotes/" + remote + "/" + branch
}

// Freshest base to branch a new tree from and how it was had: the upstream of the checked out branch, else origin's default branch,
// fetched once unless a fetch is recent; the local HEAD when none can be had
func base(repo string, fetchTimeout, fresh time.Duration) (string, string) {
	env := plain()
	try := func(timeout time.Duration, args ...string) (*git.Result, error) {
		return run(repo, env, timeout, args...)
	}
	exists := func(ref string) bool {
		result, err := try(20*time.Second, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		return err == nil && result.Code == 0
	}
	fetched := func() (time.Duration, bool) {
		dir, err := try(20*time.Second, "rev-parse", "--git-dir")
		if err != nil || dir.Code != 0 {
			return 0, false
		}
		gitDir := strings.TrimSpace(dir.Stdout)
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(repo, gitDir)
		}
		info, err := os.Stat(filepath.Join(gitDir, "FETCH_HEAD"))
		if err != nil {
			return 0, false
		}
		return max(0, time.Since(info.ModTime())), true
	}
	refresh := func(remote, branch, ref string) (string, string) {
		if age, ok := fetched(); ok && age < fresh && exists(ref) {
			return ref, fmt.Sprintf("%s (fetched %ds ago)", ref, int(age.Seconds()))
		}
		result, err := try(fetchTimeout, "fetch", remote, trackingRefspec(remote, branch))
		reason := ""
		switch _, timed := errors.AsType[*git.TimeoutError](err); {
		case err == nil && result.Code == 0:
			return ref, ref + " (fetched)"
		case err == nil:
			reason = "fetch failed"
		case timed:
			reason = fmt.Sprintf("fetch timed out after %gs", fetchTimeout.Seconds())
		default:
			reason = "fetch error: " + err.Error()
		}
		if exists(ref) {
			slog.Debug("worktree base: " + reason + " — using cached " + ref)
			return ref, fmt.Sprintf("%s (cached — %s)", ref, reason)
		}
		return "HEAD", fmt.Sprintf("HEAD (local — %s, no cached %s)", reason, ref)
	}
	upstream, err := try(20*time.Second, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if err != nil {
		slog.Debug("worktree base: upstream resolution failed", "error", err)
	} else if name := strings.TrimSpace(upstream.Stdout); upstream.Code == 0 && strings.Contains(name, "/") {
		remote, branch, _ := strings.Cut(name, "/")
		return refresh(remote, branch, name)
	}
	unreachable := "HEAD (local — could not reach remote)"
	head, err := try(20*time.Second, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD")
	if err != nil {
		slog.Debug("worktree base: default-branch resolution failed", "error", err)
		return "HEAD", unreachable
	}
	fallback := ""
	if head.Code == 0 {
		fallback = strings.Replace(strings.TrimSpace(head.Stdout), "refs/remotes/", "", 1)
	}
	if fallback == "" {
		shown, err := try(max(fetchTimeout, 5*time.Second), "remote", "show", "origin")
		if err != nil {
			slog.Debug("worktree base: default-branch resolution failed", "error", err)
			return "HEAD", unreachable
		}
		for line := range strings.Lines(shown.Stdout) {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "HEAD branch:"); ok {
				if branch := strings.TrimSpace(rest); branch != "" && branch != "(unknown)" {
					fallback = "origin/" + branch
				}
				break
			}
		}
	}
	if strings.Contains(fallback, "/") {
		remote, branch, _ := strings.Cut(fallback, "/")
		return refresh(remote, branch, fallback)
	}
	return "HEAD", unreachable
}

// Adds .worktrees/ to the repository's .gitignore when it isn't there, a failure logged at debug
func ignoreTrees(repo string) {
	path := filepath.Join(repo, ".gitignore")
	existing := ""
	if data, err := os.ReadFile(path); err == nil {
		existing = strings.ToValidUTF8(strings.TrimPrefix(string(data), bom), "�")
	} else if !errors.Is(err, os.ErrNotExist) {
		slog.Debug("Could not update .gitignore", "error", err)
		return
	}
	for line := range strings.Lines(existing) {
		if strings.TrimRight(line, "\r\n") == ".worktrees/" {
			return
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Debug("Could not update .gitignore", "error", err)
		return
	}
	defer f.Close()
	text := ".worktrees/\n"
	if existing != "" && !strings.HasSuffix(existing, "\n") {
		text = "\n" + text
	}
	if _, err := f.WriteString(text); err != nil {
		slog.Debug("Could not update .gitignore", "error", err)
	}
}

// Adds the tree on its new branch, from the local HEAD again when the base fails, every failed attempt swept: the base it was made from
// and how, and whether it was made
func add(repo, path, branch, ref, label string, say func(string)) (string, string, bool) {
	env := repoEnv(repo)
	if env == nil {
		say("✗ Failed to create worktree: could not read the repository's filter config")
		return "", "", false
	}
	create := func(settings ...string) (*git.Result, error) {
		args := append(settings, "worktree", "add", path, "-b", branch, ref)
		return run(repo, env, 2*time.Minute, args...)
	}
	result, err := create("-c", "checkout.workers=8", "-c", "checkout.thresholdForParallelism=100")
	if err == nil && result.Code != 0 {
		if ref != "HEAD" {
			slog.Warn("worktree add from "+ref+" failed; retrying from local HEAD", "error", strings.TrimSpace(result.Stderr))
			cleanupFailedAdd(repo, path, branch)
			ref, label = "HEAD", "HEAD (fallback — remote base failed)"
			result, err = create()
		}
		if err == nil && result.Code != 0 {
			cleanupFailedAdd(repo, path, branch)
			say("✗ Failed to create worktree: " + strings.TrimSpace(result.Stderr))
			return "", "", false
		}
	}
	if err != nil {
		cleanupFailedAdd(repo, path, branch)
		say("✗ Failed to create worktree: " + err.Error())
		return "", "", false
	}
	return ref, label, true
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Asks for a tree: its name, none for a generated one, the process it is locked for, and whether it is a launch's own tree, kept for the
// session working in it once the launch ends and recorded so a resume makes it again
type Request struct {
	Name   string
	Owner  int
	Launch bool
}

// Makes an isolated tree of the repository under .worktrees on a branch icebath/<name> of its own, off the freshest base when the
// settings fetch first, its .worktreeinclude entries copied in and locked for the owner: the tree, nil when it could not be made, and
// what happened saying line by line
func (w *Worktrees) Create(ctx context.Context, cfg *config.Config, repo string, request Request, sync bool, say func(string)) *Tree {
	if repo == "" {
		say("✗ --worktree requires being inside a git repository.")
		say("  cd into your project repo first, then run icebath -w")
		return nil
	}
	name := ""
	if request.Name != "" {
		name = strings.Trim(unsafeName.ReplaceAllString(request.Name, "-"), "-._")
		if runes := []rune(name); len(runes) > 40 {
			name = string(runes[:40])
		}
	}
	if name == "" {
		name = "icebath-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	}
	branch := "icebath/" + name
	dir := filepath.Join(repo, ".worktrees")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		say("✗ Failed to create worktree: " + err.Error())
		return nil
	}
	path := filepath.Join(dir, name)
	if _, err := os.Lstat(path); request.Name != "" && err == nil {
		say("✗ Worktree already exists: " + path)
		say("  Pick a different name, or remove it with: git worktree remove " + path)
		return nil
	}
	ignoreTrees(repo)
	ref, label := "HEAD", "HEAD (local — fetching before branching is off)"
	if sync {
		ref, label = base(repo, 5*time.Second, 5*time.Minute)
	}
	ref, label, ok := add(repo, path, branch, ref, label, say)
	if !ok {
		return nil
	}
	copyIncludes(repo, path)
	reason := fmt.Sprintf("icebath pid=%d", request.Owner)
	if request.Launch {
		reason = w.pidLock(request.Owner)
	}
	if _, err := run(repo, nil, 10*time.Second, "worktree", "lock", "--reason", reason, path); err != nil {
		slog.Debug("git worktree lock failed (non-fatal)", "error", err)
	} else {
		slog.Debug("Worktree locked", "tree", path, "pid", request.Owner)
	}
	w.Hooks().Recorded(ctx, repo)
	tree := &Tree{Path: path, Branch: branch, Repo: repo, Base: ref}
	if request.Launch {
		w.Record(cfg, *tree)
	}
	say("✓ Worktree created: " + path)
	say("  Branch: " + branch)
	say("  Base:   " + label)
	return tree
}
