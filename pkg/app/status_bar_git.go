package app

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const gitBranchTTLSeconds = 5.0

type gitBranchHit struct {
	at    float64
	label string
}

var (
	gitBranchCacheMu sync.Mutex
	gitBranchCache   = map[string]gitBranchHit{}
)

// Empties the per directory branch cache so the next read walks the tree again
func resetGitBranchCache() {
	gitBranchCacheMu.Lock()
	gitBranchCache = map[string]gitBranchHit{}
	gitBranchCacheMu.Unlock()
}

// Nearest enclosing git dir for start, following worktree pointer files to their private git dir
func resolveGitDir(start string) (string, bool) {
	for _, parent := range append([]string{start}, pypath.PathParents(start)...) {
		dotgit := filepath.Join(parent, ".git")
		info, err := os.Stat(dotgit)
		if err != nil {
			continue
		}
		if info.IsDir() {
			return dotgit, true
		}
		if !info.Mode().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(dotgit)
		if err != nil {
			return "", false
		}
		line := pystr.Strip(pystr.DecodeUTF8SigReplace(raw))
		if !strings.HasPrefix(line, "gitdir:") {
			return "", false
		}
		_, rest, _ := strings.Cut(line, ":")
		target, err := pypath.Resolve(pypath.PathJoin(parent, pystr.Strip(rest)))
		if err != nil {
			return "", false
		}
		if ti, err := os.Stat(target); err == nil && ti.IsDir() {
			return target, true
		}
		return "", false
	}
	return "", false
}

// Branch name for cwd or the process directory, the short commit when detached, empty outside a repository
func currentGitBranch(cwd string) string {
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return ""
		}
		cwd = wd
	}
	base, err := pypath.Resolve(cwd)
	if err != nil {
		return ""
	}
	now := monotonic()
	gitBranchCacheMu.Lock()
	hit, ok := gitBranchCache[base]
	gitBranchCacheMu.Unlock()
	if ok && now-hit.at < gitBranchTTLSeconds {
		return hit.label
	}
	label := ""
	if gitDir, ok := resolveGitDir(base); ok {
		head := ""
		if raw, err := os.ReadFile(filepath.Join(gitDir, "HEAD")); err == nil {
			head = pystr.Strip(pystr.DecodeUTF8SigReplace(raw))
		}
		if strings.HasPrefix(head, "ref:") {
			_, rest, _ := strings.Cut(head, ":")
			ref := pystr.Strip(rest)
			if strings.HasPrefix(ref, "refs/heads/") {
				label = ref[len("refs/heads/"):]
			} else {
				label = ref[strings.LastIndex(ref, "/")+1:]
			}
		} else if head != "" {
			label = pystr.Slice(head, 0, 8) + "…"
		}
	}
	gitBranchCacheMu.Lock()
	gitBranchCache[base] = gitBranchHit{at: now, label: label}
	gitBranchCacheMu.Unlock()
	return label
}
