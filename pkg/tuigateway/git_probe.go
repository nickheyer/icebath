package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/tools"
)

const gitWarmWorkers = 8

// Stripped stdout of git run in cwd, empty on any failure or a missing directory
func RunGit(cwd string, args ...string) string { return tools.GitProbeRunGit(cwd, args...) }

// Current branch name of the checkout, else its short head commit
func Branch(cwd string) string { return tools.GitProbeBranch(cwd) }

// Drops every cached root of the one process wide root cache
func InvalidateGitRoots() { tools.GitProbeInvalidate() }

// Top level repository root of cwd, empty outside a repository
func RepoRoot(cwd string) string { return tools.GitProbeRepoRoot(cwd) }

// Main repository root of cwd folding linked worktrees, in git forward slash spelling
func CommonRepoRoot(cwd string) string { return tools.GitProbeCommonRepoRoot(cwd) }

// Common and worktree roots of cwd, nil outside a repository
func ResolveGitRoots(cwd string) map[string]string {
	roots := tools.GitProbeResolve(cwd)
	if roots == nil {
		return nil
	}
	return map[string]string{"repo_root": roots["repo_root"].(string), "worktree_root": roots["worktree_root"].(string)}
}

// Resolves the roots of many cwds in parallel with a bounded pool so their results land in the cache
func WarmGitRoots(cwds []string, maxWorkers int) {
	if maxWorkers <= 0 {
		maxWorkers = gitWarmWorkers
	}
	if err := tools.GitProbeWarmRoots(cwds, maxWorkers); err != nil {
		panic(err)
	}
}
