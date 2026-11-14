package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
)

var treeNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

type Tree struct {
	Path      string
	Branch    string
	RepoRoot  string
	Base      string
	BaseLabel string
}

type CreateOptions struct {
	RepoRoot string
	Dir      string
	SyncBase bool
	Name     string
	RetainDB string
}

func (m *Manager) RepoRoot(ctx context.Context, dir string) (string, error) {
	out, ok, err := m.gitOut(ctx, dir, 5*time.Second, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		if fatal(ctx, err) {
			return "", err
		}
		return "", nil
	}
	if !ok {
		return "", nil
	}
	return normalizeGitBashPath(out), nil
}

func readTextLossy(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	return lineText([]byte(strings.ToValidUTF8(string(data), string(rune(0xFFFD))))), nil
}

func ensureWorktreesGitignored(repoRoot string) {
	gitignore := filepath.Join(repoRoot, ".gitignore")
	existing := ""
	if _, err := os.Stat(gitignore); err == nil {
		text, err := readTextLossy(gitignore)
		if err != nil {
			slog.Debug("Could not update .gitignore", "error", err)
			return
		}
		existing = text
	}
	for _, line := range strings.Split(existing, "\n") {
		if line == ".worktrees/" {
			return
		}
	}
	f, err := os.OpenFile(gitignore, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		slog.Debug("Could not update .gitignore", "error", err)
		return
	}
	defer f.Close()
	text := ""
	if existing != "" && !strings.HasSuffix(existing, "\n") {
		text = lineEnd
	}
	text += ".worktrees/" + lineEnd
	if _, err := f.WriteString(text); err != nil {
		slog.Debug("Could not update .gitignore", "error", err)
	}
}

func worktreeIncludeEntries(repoRoot string) ([]string, error) {
	path := filepath.Join(repoRoot, ".worktreeinclude")
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, nil
	}
	text, err := readTextLossy(path)
	if err != nil {
		return nil, err
	}
	var entries []string
	for _, line := range strings.Split(text, "\n") {
		entry := strings.TrimSpace(line)
		if entry != "" && !strings.HasPrefix(entry, "#") {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

func (m *Manager) copyIncludes(repoRoot, wtPath string) {
	if err := copyIncludeEntries(repoRoot, wtPath); err != nil {
		slog.Debug("Error copying .worktreeinclude entries", "error", err)
	}
}

func copyIncludeEntries(repoRoot, wtPath string) error {
	entries, err := worktreeIncludeEntries(repoRoot)
	if err != nil || len(entries) == 0 {
		return err
	}
	repoResolved := resolvePath(repoRoot)
	wtResolved := resolvePath(wtPath)
	for _, entry := range entries {
		src, dst := filepath.Join(repoRoot, entry), filepath.Join(wtPath, entry)
		srcResolved, dstResolved := resolvePath(src), resolvePath(dst)
		if !within(srcResolved, repoResolved) {
			slog.Warn("Skipping .worktreeinclude entry outside repo root", "entry", entry)
			continue
		}
		if !within(dstResolved, wtResolved) {
			slog.Warn("Skipping .worktreeinclude entry that escapes worktree", "entry", entry)
			continue
		}
		info, err := os.Stat(src)
		if err != nil {
			continue
		}
		if info.Mode().IsRegular() {
			if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
				return err
			}
			if err := fileutil.CopyFile(src, dst); err != nil {
				return err
			}
			continue
		}
		if !info.IsDir() {
			continue
		}
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
			return err
		}
		if err := os.Symlink(srcResolved, dst); err != nil {
			if !symlinkFallsBackToCopy() {
				return err
			}
			slog.Info(".worktreeinclude: symlink failed, falling back to a copy", "error", err)
			if cerr := fileutil.CopyTree(srcResolved, dst, fileutil.CopyTreeOptions{Symlinks: true}); cerr != nil {
				slog.Warn(".worktreeinclude: copy fallback also failed", "src", src, "dst", dst, "error", cerr)
			}
		}
	}
	return nil
}

func (m *Manager) resolveBase(ctx context.Context, repoRoot string, fetchTimeout, freshness time.Duration) (string, string, error) {
	env := m.noninteractiveEnv(ctx)
	gitRun := func(timeout time.Duration, args ...string) (*result, error) {
		return m.git(ctx, repoRoot, timeout, env, args...)
	}
	refExists := func(ref string) (bool, error) {
		r, err := gitRun(20*time.Second, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		if err != nil {
			if fatal(ctx, err) {
				return false, err
			}
			return false, nil
		}
		return r.ok(), nil
	}
	fetchHeadAge := func() (time.Duration, bool, error) {
		gd, err := gitRun(20*time.Second, "rev-parse", "--git-dir")
		if err != nil {
			if fatal(ctx, err) {
				return 0, false, err
			}
			return 0, false, nil
		}
		if !gd.ok() {
			return 0, false, nil
		}
		info, err := os.Stat(filepath.Join(joinPath(repoRoot, gd.out()), "FETCH_HEAD"))
		if err != nil {
			return 0, false, nil
		}
		return max(0, time.Since(info.ModTime())), true, nil
	}
	refresh := func(remote, branch, ref string) (string, string, error) {
		age, known, err := fetchHeadAge()
		if err != nil {
			return "", "", err
		}
		if known && age < freshness {
			exists, err := refExists(ref)
			if err != nil {
				return "", "", err
			}
			if exists {
				return ref, fmt.Sprintf("%s (fetched %ds ago)", ref, int(age.Seconds())), nil
			}
		}
		reason := ""
		fetched, err := gitRun(fetchTimeout, "fetch", remote, "+refs/heads/"+branch+":refs/remotes/"+remote+"/"+branch)
		var timeout *DeadlineError
		switch {
		case err == nil && fetched.ok():
			return ref, ref + " (fetched)", nil
		case err == nil:
			reason = "fetch failed"
		case fatal(ctx, err):
			return "", "", err
		case errors.As(err, &timeout):
			reason = fmt.Sprintf("fetch timed out after %gs", fetchTimeout.Seconds())
		default:
			reason = "fetch error: " + err.Error()
		}
		exists, err := refExists(ref)
		if err != nil {
			return "", "", err
		}
		if exists {
			slog.Debug("worktree base: using cached ref", "reason", reason, "ref", ref)
			return ref, ref + " (cached — " + reason + ")", nil
		}
		return "HEAD", "HEAD (local — " + reason + ", no cached " + ref + ")", nil
	}
	up, err := gitRun(20*time.Second, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	switch {
	case err != nil && fatal(ctx, err):
		return "", "", err
	case err != nil:
		slog.Debug("worktree base: upstream resolution failed", "error", err)
	case up.ok():
		upstream := up.out()
		if remote, branch, found := strings.Cut(upstream, "/"); upstream != "" && found {
			return refresh(remote, branch, upstream)
		}
	}
	ref, label, done, err := m.defaultBranchBase(ctx, gitRun, fetchTimeout, refresh)
	if err != nil || done {
		return ref, label, err
	}
	return "HEAD", "HEAD (local — could not reach remote)", nil
}

func (m *Manager) defaultBranchBase(ctx context.Context, gitRun func(time.Duration, ...string) (*result, error), fetchTimeout time.Duration, refresh func(remote, branch, ref string) (string, string, error)) (string, string, bool, error) {
	headRef, err := gitRun(20*time.Second, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD")
	if err != nil {
		if fatal(ctx, err) {
			return "", "", false, err
		}
		slog.Debug("worktree base: default-branch resolution failed", "error", err)
		return "", "", false, nil
	}
	defaultRef := ""
	if headRef.ok() {
		defaultRef = strings.Replace(headRef.out(), "refs/remotes/", "", 1)
	}
	if defaultRef == "" {
		show, err := gitRun(max(fetchTimeout, 5*time.Second), "remote", "show", "origin")
		if err != nil {
			if fatal(ctx, err) {
				return "", "", false, err
			}
			slog.Debug("worktree base: default-branch resolution failed", "error", err)
			return "", "", false, nil
		}
		for _, line := range strings.Split(show.text(), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "HEAD branch:") {
				_, b, _ := strings.Cut(line, ":")
				b = strings.TrimSpace(b)
				if b != "" && b != "(unknown)" {
					defaultRef = "origin/" + b
				}
				break
			}
		}
	}
	if remote, branch, found := strings.Cut(defaultRef, "/"); defaultRef != "" && found {
		ref, label, err := refresh(remote, branch, defaultRef)
		return ref, label, err == nil, err
	}
	return "", "", false, nil
}

func (m *Manager) cleanupFailedAdd(ctx context.Context, repoRoot, wtPath, branch string) error {
	if err := m.gitQuiet(ctx, repoRoot, 15*time.Second, nil, "", "worktree", "unlock", wtPath); err != nil {
		return err
	}
	if err := m.gitQuiet(ctx, repoRoot, 15*time.Second, nil, "", "worktree", "remove", "--force", wtPath); err != nil {
		return err
	}
	if _, err := os.Lstat(wtPath); err == nil {
		if rerr := fileutil.RemoveTree(wtPath, false); rerr != nil {
			slog.Debug("cleanup after failed worktree add", "error", rerr)
		}
	}
	env := m.noninteractiveEnv(ctx)
	if err := m.gitQuiet(ctx, repoRoot, 15*time.Second, env, "", "worktree", "prune"); err != nil {
		return err
	}
	return m.gitQuiet(ctx, repoRoot, 15*time.Second, env, "", "branch", "-D", branch)
}

func (m *Manager) worktreeAdd(ctx context.Context, repoRoot, wtPath, branch, baseRef, baseLabel string) (string, string, error) {
	env, trusted, err := m.repoEnv(ctx, repoRoot)
	if err != nil {
		return "", "", err
	}
	if !trusted {
		return "", "", &CreateError{Msg: "Failed to create worktree: could not read the repository's filter config"}
	}
	add := func(cfg ...string) (*result, error) {
		args := append(cfg, "worktree", "add", wtPath, "-b", branch, baseRef)
		return m.git(ctx, repoRoot, 120*time.Second, env, args...)
	}
	fail := func(cause error, msg string) (string, string, error) {
		if cerr := m.cleanupFailedAdd(ctx, repoRoot, wtPath, branch); cerr != nil {
			return "", "", cerr
		}
		if fatal(ctx, cause) {
			return "", "", cause
		}
		return "", "", &CreateError{Msg: "Failed to create worktree: " + msg, Err: cause}
	}
	r, err := add("-c", "checkout.workers=8", "-c", "checkout.thresholdForParallelism=100")
	if err != nil {
		return fail(err, err.Error())
	}
	if !r.ok() {
		if baseRef != "HEAD" {
			slog.Warn("worktree add failed; retrying from local HEAD", "base", baseRef, "stderr", r.errText())
			if cerr := m.cleanupFailedAdd(ctx, repoRoot, wtPath, branch); cerr != nil {
				return "", "", cerr
			}
			baseRef, baseLabel = "HEAD", "HEAD (fallback — remote base failed)"
			r, err = add()
			if err != nil {
				return fail(err, err.Error())
			}
		}
		if !r.ok() {
			return fail(nil, r.errText())
		}
	}
	return baseRef, baseLabel, nil
}

func (m *Manager) Create(ctx context.Context, o CreateOptions) (*Tree, error) {
	repoRoot := o.RepoRoot
	if repoRoot == "" {
		var err error
		if repoRoot, err = m.RepoRoot(ctx, o.Dir); err != nil {
			return nil, err
		}
	}
	if repoRoot == "" {
		return nil, &CreateError{Msg: "--worktree requires being inside a git repository.", Hint: "cd into your project repo first, then run icebath -w"}
	}
	name := ""
	if o.Name != "" {
		name = strings.Trim(treeNameUnsafe.ReplaceAllString(o.Name, "-"), "-._")
		if len(name) > 40 {
			name = name[:40]
		}
	}
	if name == "" {
		suffix, err := randomHex(8)
		if err != nil {
			return nil, err
		}
		name = "icebath-" + suffix
	}
	branch := "icebath/" + name
	worktreesDir := filepath.Join(repoRoot, ".worktrees")
	if err := os.MkdirAll(worktreesDir, 0o777); err != nil {
		return nil, err
	}
	wtPath := filepath.Join(worktreesDir, name)
	if o.Name != "" {
		if _, err := os.Lstat(wtPath); err == nil {
			return nil, &CreateError{Msg: "Worktree already exists: " + wtPath, Hint: "Pick a different name, or remove it with: git worktree remove " + wtPath}
		}
	}
	ensureWorktreesGitignored(repoRoot)
	baseRef, baseLabel := "HEAD", "HEAD (local — worktree_sync disabled)"
	if o.SyncBase {
		var err error
		if baseRef, baseLabel, err = m.resolveBase(ctx, repoRoot, 5*time.Second, 300*time.Second); err != nil {
			return nil, err
		}
	}
	baseRef, baseLabel, err := m.worktreeAdd(ctx, repoRoot, wtPath, branch, baseRef, baseLabel)
	if err != nil {
		return nil, err
	}
	m.copyIncludes(repoRoot, wtPath)
	reason := fmt.Sprintf("icebath pid=%d", os.Getpid())
	if o.RetainDB != "" {
		reason += " db=" + o.RetainDB
	}
	if err := m.gitQuiet(ctx, repoRoot, 10*time.Second, nil, "git worktree lock failed (non-fatal)", "worktree", "lock", "--reason", reason, wtPath); err != nil {
		return nil, err
	}
	if err := m.recordQuietly(ctx, repoRoot); err != nil {
		return nil, err
	}
	tree := &Tree{Path: wtPath, Branch: branch, RepoRoot: repoRoot, Base: baseRef, BaseLabel: baseLabel}
	if o.RetainDB != "" {
		if err := m.RecordTUITree(ctx, tree); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			slog.Debug("TUI worktree record failed", "path", wtPath, "error", err)
		}
	}
	return tree, nil
}
