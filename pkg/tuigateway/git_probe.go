package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/tools"
)

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
