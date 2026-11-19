package tuihost

import (
	"github.com/nickheyer/icebath/pkg/tools"
)

// Current branch name of the checkout, else its short head commit
func Branch(cwd string) string { return tools.GitProbeBranch(cwd) }

// Top level repository root of cwd, empty outside a repository
func RepoRoot(cwd string) string { return tools.GitProbeRepoRoot(cwd) }

// Main repository root of cwd folding linked worktrees, in git forward slash spelling
func CommonRepoRoot(cwd string) string { return tools.GitProbeCommonRepoRoot(cwd) }
