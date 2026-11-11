package tuigateway

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	gitProbeTimeout = 1500 * time.Millisecond
	gitWarmWorkers  = 8
	gitNegativeTTL  = 30 * time.Second
)

// Stripped stdout of git run in cwd, empty on any failure or a missing directory
func RunGit(cwd string, args ...string) string {
	if cwd == "" {
		return ""
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return ""
	}
	return tools.BoundedGitProbe(append([]string{"git", "-C", cwd}, args...), gitProbeTimeout, nil)
}

// Current branch name of the checkout, else its short head commit
func Branch(cwd string) string {
	if name := RunGit(cwd, "branch", "--show-current"); name != "" {
		return name
	}
	return RunGit(cwd, "rev-parse", "--short", "HEAD")
}

// Single flight cache of root probes, positives kept for the process and negatives for a short TTL
type rootCache struct {
	mu       sync.Mutex
	roots    map[string]string
	neg      map[string]time.Time
	inflight map[string]chan struct{}
}

// Drops every cached root, negative and in flight gate
func (c *rootCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.roots, c.neg, c.inflight = map[string]string{}, map[string]time.Time{}, map[string]chan struct{}{}
}

// Cached root of key, probed once by a leader while followers wait and re read
func (c *rootCache) resolve(key string, probe func() string) string {
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
			case <-time.After(gitProbeTimeout + 500*time.Millisecond):
			}
			continue
		}
		value := ""
		func() {
			defer func() {
				c.mu.Lock()
				if value != "" {
					c.roots[key] = value
				} else {
					c.neg[key] = time.Now().Add(gitNegativeTTL)
				}
				delete(c.inflight, key)
				c.mu.Unlock()
				close(gate)
			}()
			value = probe()
		}()
		return value
	}
}

var gitRoots = &rootCache{roots: map[string]string{}, neg: map[string]time.Time{}, inflight: map[string]chan struct{}{}}

// Drops cached roots after a known mutation such as an added worktree
func InvalidateGitRoots() { gitRoots.invalidate() }

// Top level repository root of cwd, empty outside a repository
func RepoRoot(cwd string) string {
	if cwd == "" {
		return ""
	}
	return gitRoots.resolve(cwd, func() string { return RunGit(cwd, "rev-parse", "--show-toplevel") })
}

// Main repository root of cwd folding linked worktrees, in git forward slash spelling
func CommonRepoRoot(cwd string) string {
	if cwd == "" || RepoRoot(cwd) == "" {
		return ""
	}
	return gitRoots.resolve("common:"+cwd, func() string {
		gitdir := RunGit(cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if gitdir != "" {
			if real, err := filepath.EvalSymlinks(gitdir); err == nil {
				gitdir = real
			} else if abs, err := filepath.Abs(gitdir); err == nil {
				gitdir = abs
			}
			if filepath.Base(gitdir) == ".git" {
				return strings.ReplaceAll(filepath.Dir(gitdir), string(os.PathSeparator), "/")
			}
		}
		return RepoRoot(cwd)
	})
}

// Common and worktree roots of cwd, nil outside a repository
func ResolveGitRoots(cwd string) map[string]string {
	worktree := RepoRoot(cwd)
	if worktree == "" {
		return nil
	}
	common := CommonRepoRoot(cwd)
	if common == "" {
		common = worktree
	}
	return map[string]string{"repo_root": common, "worktree_root": worktree}
}

// Resolves the roots of many cwds in parallel with a bounded pool so their results land in the cache
func WarmGitRoots(cwds []string, maxWorkers int) {
	if maxWorkers <= 0 {
		maxWorkers = gitWarmWorkers
	}
	seen := map[string]bool{}
	var pending []string
	for _, cwd := range cwds {
		if trimmed := strings.TrimSpace(cwd); trimmed != "" && !seen[trimmed] {
			seen[trimmed] = true
			pending = append(pending, trimmed)
		}
	}
	sort.Strings(pending)
	if len(pending) == 1 {
		ResolveGitRoots(pending[0])
		return
	}
	slots := make(chan struct{}, min(maxWorkers, len(pending)))
	var wg sync.WaitGroup
	for _, cwd := range pending {
		wg.Add(1)
		slots <- struct{}{}
		go func(cwd string) {
			defer wg.Done()
			defer func() { <-slots }()
			ResolveGitRoots(cwd)
		}(cwd)
	}
	wg.Wait()
}
