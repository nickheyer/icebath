package tools

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Stops a prune that has no permission to discard another snapshot
type CheckpointPruneError struct{ Msg string }

func (e *CheckpointPruneError) Error() string { return e.Msg }

// Marker that tells the next periodic prune a ref rewrite skipped its gc
const checkpointGCPendingName = ".gc-pending"

// Per call knobs of a git run against the shadow store, a zero timeout means the configured default
type checkpointGitOpts struct {
	Timeout   time.Duration
	Allowed   map[int]bool
	IndexFile string
	ExtraEnv  map[string]string
}

// Git runner shape shared by the manager and the pruner, ok with trimmed stdout and stderr
type checkpointGitFunc func(args []string, store, cwd string, opts checkpointGitOpts) (bool, string, string)

// Lock file beside the resolved base so clearing the base never replaces its inode
func checkpointStoreLockPath(base string) string {
	base = realPath(base)
	return filepath.Join(filepath.Dir(base), "."+filepath.Base(base)+".lock")
}

// Takes the whole store lock without waiting and returns its release, a busy store is a prune error
func checkpointStoreLock(base string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(base), 0o777); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(checkpointStoreLockPath(base), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ok, err := checkpointTryLock(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	if !ok {
		f.Close()
		return nil, &CheckpointPruneError{Msg: "checkpoint store is busy: " + base}
	}
	return func() { f.Close() }, nil
}

// Rewrites checkpoint histories for the snapshot count and size budgets
type CheckpointPruner struct {
	RunGit     checkpointGitFunc
	Store      string
	Cwd        string
	Timeout    int
	DirSize    func(string) int64
	RefsPrefix string
}

// Output of a git step that must succeed, a failed step is a prune error
func (p *CheckpointPruner) git(args []string, opts checkpointGitOpts) (string, error) {
	ok, output, errText := p.RunGit(args, p.Store, p.Cwd, opts)
	if !ok {
		return "", &CheckpointPruneError{Msg: "checkpoint " + args[0] + " failed: " + errText}
	}
	return output, nil
}

// Expires every reflog and runs a full gc, then restores the bare store directories and clears the marker
func (p *CheckpointPruner) Reclaim() error {
	if _, err := p.git([]string{"reflog", "expire", "--expire=now", "--all"}, checkpointGitOpts{}); err != nil {
		return err
	}
	gcOpts := checkpointGitOpts{Timeout: time.Duration(p.Timeout*3) * time.Second}
	if _, err := p.git([]string{"gc", "--prune=now", "--quiet"}, gcOpts); err != nil {
		return err
	}
	for _, name := range []string{filepath.Join("refs", "heads"), "branches"} {
		if err := os.MkdirAll(filepath.Join(p.Store, name), 0o777); err != nil {
			return err
		}
	}
	os.Remove(filepath.Join(p.Store, checkpointGCPendingName))
	return nil
}

// Reports whether a deferred rewrite still waits for its gc
func (p *CheckpointPruner) GCPending() bool {
	_, err := os.Stat(filepath.Join(p.Store, checkpointGCPendingName))
	return err == nil
}

// Leaves the deferred gc marker, ignoring a store that cannot be written
func (p *CheckpointPruner) markGCPending() {
	path := filepath.Join(p.Store, checkpointGCPendingName)
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o666); err == nil {
		f.Close()
		now := time.Now()
		os.Chtimes(path, now, now)
	}
}

// Commits of the ref oldest first, an empty history is a prune error
func (p *CheckpointPruner) commits(ref string) ([]string, error) {
	out, err := p.git([]string{"rev-list", "--reverse", ref}, checkpointGitOpts{})
	if err != nil {
		return nil, err
	}
	commits := pySplitLines(out)
	if len(commits) == 0 {
		return nil, &CheckpointPruneError{Msg: "checkpoint ref has no readable history: " + ref}
	}
	return commits, nil
}

// Rebuilds the kept commits with their trees and dates, then moves the ref only if it did not change meanwhile
func (p *CheckpointPruner) rewrite(ref string, original, keep []string, gc bool) error {
	parent := ""
	haveParent := false
	for _, sha := range keep {
		metadata, err := p.git([]string{"log", "-1", "--format=%T%x00%aI%x00%cI%x00%s", sha}, checkpointGitOpts{})
		if err != nil {
			return err
		}
		fields := strings.SplitN(metadata, "\x00", 4)
		if len(fields) != 4 {
			return &CheckpointPruneError{Msg: "invalid checkpoint metadata: " + sha}
		}
		args := []string{"commit-tree", fields[0], "--no-gpg-sign", "-m", fields[3]}
		if haveParent {
			args = append(args, "-p", parent)
		}
		dates := map[string]string{"GIT_AUTHOR_DATE": fields[1], "GIT_COMMITTER_DATE": fields[2]}
		parent, err = p.git(args, checkpointGitOpts{ExtraEnv: dates})
		if err != nil {
			return err
		}
		haveParent = true
		if parent == "" {
			return &CheckpointPruneError{Msg: "commit-tree returned no checkpoint identity"}
		}
	}
	if !haveParent {
		return &CheckpointPruneError{Msg: "cannot remove a project's last snapshot"}
	}
	if _, err := p.git([]string{"update-ref", ref, parent, original[len(original)-1]}, checkpointGitOpts{}); err != nil {
		return err
	}
	if gc {
		return p.Reclaim()
	}
	p.markGCPending()
	return nil
}

// Keeps the newest snapshots of the ref within the count budget and defers the gc
func (p *CheckpointPruner) Trim(ref string, keepCount int) error {
	commits, err := p.commits(ref)
	if err != nil {
		return err
	}
	keepCount = max(1, keepCount)
	if len(commits) > keepCount {
		return p.rewrite(ref, commits, commits[len(commits)-keepCount:], false)
	}
	return nil
}

// Every project ref under the prefix
func (p *CheckpointPruner) refs() ([]string, error) {
	out, err := p.git([]string{"for-each-ref", "--format=%(refname)", p.RefsPrefix}, checkpointGitOpts{})
	if err != nil {
		return nil, err
	}
	return pySplitLines(out), nil
}

// Drops the oldest snapshot of every project once when the store is over the cap, deferring the gc
func (p *CheckpointPruner) DropOneRound(capBytes int64) (bool, error) {
	if capBytes <= 0 || p.DirSize(p.Store) <= capBytes {
		return false, nil
	}
	refs, err := p.refs()
	if err != nil {
		return false, err
	}
	changed := false
	for _, ref := range refs {
		commits, err := p.commits(ref)
		if err != nil {
			return changed, err
		}
		if len(commits) > 1 {
			if err := p.rewrite(ref, commits, commits[1:], false); err != nil {
				return changed, err
			}
			changed = true
		}
	}
	return changed, nil
}

// Runs gc then drops oldest snapshots round robin, remeasuring after each rewrite, for at most twenty rounds
func (p *CheckpointPruner) EnforceSize(capBytes int64) (bool, error) {
	if capBytes <= 0 || p.DirSize(p.Store) <= capBytes {
		return true, nil
	}
	if err := p.Reclaim(); err != nil {
		return false, err
	}
	if p.DirSize(p.Store) <= capBytes {
		return true, nil
	}
	refs, err := p.refs()
	if err != nil {
		return false, err
	}
	for range 20 {
		changed := false
		for _, ref := range refs {
			commits, err := p.commits(ref)
			if err != nil {
				return false, err
			}
			if len(commits) <= 1 {
				continue
			}
			if err := p.rewrite(ref, commits, commits[1:], true); err != nil {
				return false, err
			}
			changed = true
			if p.DirSize(p.Store) <= capBytes {
				return true, nil
			}
		}
		if !changed {
			break
		}
	}
	return false, nil
}
