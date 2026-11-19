package tools

import (
	"regexp"
	"runtime"
	"strings"
	"time"
)

var gitBashPathRe = regexp.MustCompile(`^/(?:(?:cygdrive|mnt)/)?([a-zA-Z])/(.*)$`)

// Translates a Git Bash drive path to its Windows form, every other path unchanged
func normalizeGitBashPath(p string) string {
	if p == "" || runtime.GOOS != "windows" {
		return p
	}
	m := gitBashPathRe.FindStringSubmatch(p)
	if m == nil {
		return p
	}
	return strings.ToUpper(m[1]) + ":\\" + strings.ReplaceAll(m[2], "/", "\\")
}

// Repository root of a directory in Git Bash normalized form, the working directory when empty, empty outside a repo
func GitRepoRootOf(cwd string) string {
	out, ok, err := worktreeGitOut([]string{"rev-parse", "--show-toplevel"}, cwd, 5*time.Second)
	if err != nil || !ok {
		return ""
	}
	return normalizeGitBashPath(out)
}

// Git repo root of the working directory in Git Bash normalized form, empty outside a repo
func gitRepoRoot() string { return GitRepoRootOf("") }

// GitRepoRoot returns the repository root of the working directory, empty outside a repository
func GitRepoRoot() string { return gitRepoRoot() }
