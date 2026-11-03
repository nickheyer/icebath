package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/tool"
	"github.com/nickheyer/icebath/pkg/tool/files"
	"github.com/nickheyer/icebath/pkg/tool/shell"
)

// Directory under the project state holding the worktrees of isolated agents
const worktreesDir = "worktrees"

// Temporary git worktree an isolated agent works in, with the tools rooted there
type worktree struct {
	Path   string
	Base   string
	Branch string
	tools  *tool.Registry
	sh     *shell.Shell
	// The path came from a hook, so the hook owns its removal
	hooked bool
}

// Runs git in a directory and returns its output
func gitIn(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errs.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// Default branch of the repository, main or master when the remote names none
func defaultBranch(ctx context.Context, root string) string {
	if ref, err := gitIn(ctx, root, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && ref != "" {
		return strings.TrimPrefix(ref, "origin/")
	}
	for _, name := range []string{"main", "master"} {
		if _, err := gitIn(ctx, root, "rev-parse", "--verify", "--quiet", name); err == nil {
			return name
		}
	}
	branch, err := gitIn(ctx, root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "HEAD"
	}
	return branch
}

// Creates the worktree an isolated agent works in, a WorktreeCreate hook able to supply the path instead
func (a *App) createWorktree(ctx context.Context, sess *session.Session, name string) (*worktree, error) {
	if !inGitRepo(a.Root) {
		return nil, errors.New("worktree isolation needs a git repository")
	}
	state, err := StatePath(a.Root)
	if err != nil {
		return nil, err
	}
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, name)
	path := filepath.Join(state, worktreesDir, fmt.Sprintf("%s-%d", safe, time.Now().UnixNano()))
	v, err := a.FireSession(ctx, sess, agent.WorktreeCreate, "", map[string]any{"name": safe, "path": path})
	if err != nil {
		return nil, err
	}
	if v.Block {
		return nil, fmt.Errorf("worktree creation blocked by hook: %s", v.Reason)
	}
	wt := &worktree{Base: a.Root}
	if v.Path != "" {
		if !filepath.IsAbs(v.Path) {
			return nil, fmt.Errorf("WorktreeCreate hook returned a relative path %q", v.Path)
		}
		if st, err := os.Stat(v.Path); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("WorktreeCreate hook returned %s, which is not a directory", v.Path)
		}
		wt.Path, wt.hooked = v.Path, true
	} else {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		wt.Branch = defaultBranch(ctx, a.Root)
		if _, err := gitIn(ctx, a.Root, "worktree", "add", "--detach", path, wt.Branch); err != nil {
			return nil, err
		}
		wt.Path = path
	}
	if err := a.rootTools(wt); err != nil {
		a.removeWorktree(ctx, sess, wt)
		return nil, err
	}
	return wt, nil
}

// Gives a worktree its own shell and file tools, every other tool of the app shared
func (a *App) rootTools(wt *worktree) error {
	cfg := a.Config
	sh, err := shell.New(shell.Options{Dir: wt.Path, Env: cfg.Shell.Env, Timeout: seconds(cfg.Shell.Timeout), MaxTimeout: seconds(cfg.Shell.MaxTimeout), OnJob: func(ev shell.JobEvent) { a.send(JobMsg{Event: ev}) }})
	if err != nil {
		return err
	}
	reg := &tool.Registry{Timeout: a.Tools.Timeout}
	if err := reg.Add(files.All(files.Options{Dir: wt.Path})...); err != nil {
		return err
	}
	if err := reg.Add(sh.Tools()...); err != nil {
		return err
	}
	own := map[string]bool{}
	for _, n := range reg.Names() {
		own[n] = true
	}
	for _, n := range a.Tools.Names() {
		if own[n] {
			continue
		}
		t, ok := a.Tools.Lookup(n)
		if !ok {
			continue
		}
		if err := reg.Add(t); err != nil {
			return err
		}
	}
	for _, n := range a.Tools.Disabled() {
		reg.SetEnabled(n, false)
	}
	wt.tools, wt.sh = reg, sh
	return nil
}

// Reports whether the worktree holds no change, no modified file and no commit beyond its base
func (wt *worktree) clean(ctx context.Context) bool {
	status, err := gitIn(ctx, wt.Path, "status", "--porcelain")
	if err != nil || status != "" {
		return false
	}
	if wt.Branch == "" {
		return true
	}
	ahead, err := gitIn(ctx, wt.Path, "rev-list", "--count", wt.Branch+"..HEAD")
	return err == nil && ahead == "0"
}

// Removes a worktree, firing WorktreeRemove, a hook owned path left to the hook
func (a *App) removeWorktree(ctx context.Context, sess *session.Session, wt *worktree) error {
	if wt.sh != nil {
		wt.sh.Close()
	}
	_, err := a.FireSession(ctx, sess, agent.WorktreeRemove, "", map[string]any{"path": wt.Path, "name": filepath.Base(wt.Path)})
	if err != nil {
		return err
	}
	if wt.hooked {
		if _, err := os.Stat(wt.Path); err == nil {
			return fmt.Errorf("worktree %s still exists after its WorktreeRemove hooks ran", wt.Path)
		}
		return nil
	}
	if _, err := gitIn(ctx, wt.Base, "worktree", "remove", "--force", wt.Path); err != nil {
		os.RemoveAll(wt.Path)
		gitIn(ctx, wt.Base, "worktree", "prune")
	}
	return nil
}

// Ends the worktree of an isolated agent after its run, kept with its changes and removed when nothing changed
func (a *App) finishWorktree(ctx context.Context, sess *session.Session, wt *worktree) string {
	if wt == nil {
		return ""
	}
	if wt.clean(ctx) {
		if err := a.removeWorktree(ctx, sess, wt); err != nil {
			a.send(NoticeMsg{Level: Warn, Text: err.Error()})
		}
		return ""
	}
	if wt.sh != nil {
		wt.sh.Close()
	}
	return wt.Path
}
