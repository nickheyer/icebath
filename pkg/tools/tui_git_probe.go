package tools

import (
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

const (
	tuiGitTimeout     = 1500 * time.Millisecond
	tuiGitWarmWorkers = 8
)

// How long a not a repo answer is trusted so a fresh git init shows within seconds
var tuiGitNegTTL = 30 * time.Second

// Git command the root probes run, swapped by tests under its lock
var (
	tuiRunGitMu   sync.RWMutex
	tuiRunGitHook = runTUIGit
)

// Runs git through the current probe runner, read under its lock so a swap never races a probe
func tuiRunGit(cwd string, args ...string) string {
	tuiRunGitMu.RLock()
	run := tuiRunGitHook
	tuiRunGitMu.RUnlock()
	return run(cwd, args...)
}

// Installs a git probe runner and returns the one it replaced
func setTUIRunGit(fn func(cwd string, args ...string) string) func(cwd string, args ...string) string {
	tuiRunGitMu.Lock()
	defer tuiRunGitMu.Unlock()
	saved := tuiRunGitHook
	tuiRunGitHook = fn
	return saved
}

// Stripped stdout of git -C cwd args, empty for a missing directory or any failure
func runTUIGit(cwd string, args ...string) string {
	if cwd == "" {
		return ""
	}
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return ""
	}
	return boundedGitProbe(append([]string{"git", "-C", cwd}, args...), tuiGitTimeout, nil)
}

// GitProbeRunGit runs one bounded git probe in cwd for the TUI gateway
func GitProbeRunGit(cwd string, args ...string) string {
	return tuiRunGit(cwd, args...)
}

// GitProbeBranch is the current branch name, else the short HEAD hash
func GitProbeBranch(cwd string) string {
	if b := tuiRunGit(cwd, "branch", "--show-current"); b != "" {
		return b
	}
	return tuiRunGit(cwd, "rev-parse", "--short", "HEAD")
}

// Single flight root cache keeping positives for the process and negatives for the TTL
type tuiRootCache struct {
	mu       sync.Mutex
	roots    map[string]string
	neg      map[string]time.Time
	inflight map[string]chan struct{}
}

func newTUIRootCache() *tuiRootCache {
	return &tuiRootCache{roots: map[string]string{}, neg: map[string]time.Time{}, inflight: map[string]chan struct{}{}}
}

// Drops every cached root, negative and in flight gate
func (c *tuiRootCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.roots)
	clear(c.neg)
	clear(c.inflight)
}

// Cached root for key, the first caller probing while followers wait on its gate and reread
func (c *tuiRootCache) resolve(key string, probe func() string) string {
	for {
		c.mu.Lock()
		if hit := c.roots[key]; hit != "" {
			c.mu.Unlock()
			return hit
		}
		if expiry, ok := c.neg[key]; ok {
			if expiry.After(time.Now()) {
				c.mu.Unlock()
				return ""
			}
			delete(c.neg, key)
		}
		gate, following := c.inflight[key]
		if !following {
			gate = make(chan struct{})
			c.inflight[key] = gate
		}
		c.mu.Unlock()
		if following {
			select {
			case <-gate:
			case <-time.After(tuiGitTimeout + 500*time.Millisecond):
			}
			continue
		}
		return c.lead(key, gate, probe)
	}
}

// Runs the probe as leader, recording its answer and opening the gate even when the probe panics
func (c *tuiRootCache) lead(key string, gate chan struct{}, probe func() string) (value string) {
	defer func() {
		c.mu.Lock()
		if value != "" {
			c.roots[key] = value
		} else {
			c.neg[key] = time.Now().Add(tuiGitNegTTL)
		}
		delete(c.inflight, key)
		c.mu.Unlock()
		close(gate)
	}()
	return probe()
}

var tuiGitRoots = newTUIRootCache()

// GitProbeInvalidate drops cached roots after a known mutation such as a new worktree
func GitProbeInvalidate() {
	tuiGitRoots.invalidate()
}

// GitProbeRepoRoot is the top level repository root of cwd, empty outside a repository
func GitProbeRepoRoot(cwd string) string {
	if cwd == "" {
		return ""
	}
	return tuiGitRoots.resolve(cwd, func() string { return tuiRunGit(cwd, "rev-parse", "--show-toplevel") })
}

// GitProbeCommonRepoRoot is the main checkout root folding linked worktrees, in forward slash spelling
func GitProbeCommonRepoRoot(cwd string) string {
	if cwd == "" || GitProbeRepoRoot(cwd) == "" {
		return ""
	}
	return tuiGitRoots.resolve("common:"+cwd, func() string {
		if gitdir := tuiRunGit(cwd, "rev-parse", "--path-format=absolute", "--git-common-dir"); gitdir != "" {
			gitdir, _ = pypath.Realpath(gitdir, pypath.Lax)
			if pypath.Basename(gitdir) == ".git" {
				return strings.ReplaceAll(pypath.Dirname(gitdir), string(os.PathSeparator), "/")
			}
		}
		return GitProbeRepoRoot(cwd)
	})
}

// GitProbeResolve maps cwd to its repo_root and worktree_root, nil outside a repository
func GitProbeResolve(cwd string) map[string]any {
	worktree := GitProbeRepoRoot(cwd)
	if worktree == "" {
		return nil
	}
	root := GitProbeCommonRepoRoot(cwd)
	if root == "" {
		root = worktree
	}
	return map[string]any{"repo_root": root, "worktree_root": worktree}
}

// GitProbeWarmRoots resolves every distinct cwd in parallel on at most maxWorkers goroutines
func GitProbeWarmRoots(cwds []string, maxWorkers int) error {
	seen := map[string]bool{}
	var pending []string
	for _, cwd := range cwds {
		if cwd = strings.TrimFunc(cwd, pyIsSpace); cwd != "" && !seen[cwd] {
			seen[cwd] = true
			pending = append(pending, cwd)
		}
	}
	sort.Strings(pending)
	switch {
	case len(pending) == 1:
		GitProbeResolve(pending[0])
		return nil
	case len(pending) == 0:
		return nil
	}
	workers := min(maxWorkers, len(pending))
	if workers <= 0 {
		return errors.New("max_workers must be greater than 0")
	}
	jobs := make(chan string)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for cwd := range jobs {
				GitProbeResolve(cwd)
			}
		})
	}
	for _, cwd := range pending {
		jobs <- cwd
	}
	close(jobs)
	wg.Wait()
	return nil
}
