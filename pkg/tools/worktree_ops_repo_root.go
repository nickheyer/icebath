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

// Git repo root of the working directory in Git Bash normalized form, empty outside a repo
func gitRepoRoot() string {
	out, ok, err := worktreeGitOut([]string{"rev-parse", "--show-toplevel"}, "", 5*time.Second)
	if err != nil || !ok {
		return ""
	}
	return normalizeGitBashPath(out)
}

// GitRepoRoot returns the repository root of the working directory, empty outside a repository
func GitRepoRoot() string { return gitRepoRoot() }

// Path components the way pathlib keeps them, empty and dot parts dropped, the anchor kept
func purePathParts(p string) []string {
	if runtime.GOOS == "windows" {
		p = strings.ReplaceAll(p, "\\", "/")
	}
	parts := []string{}
	if strings.HasPrefix(p, "/") {
		parts = append(parts, "/")
	}
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return parts
}

// Whether a path stays within the root, a lexical component prefix check like pathlib relative_to
func pathIsWithinRoot(path, root string) bool {
	pp, rp := purePathParts(path), purePathParts(root)
	if len(rp) > len(pp) {
		return false
	}
	for i, part := range rp {
		if runtime.GOOS == "windows" {
			if !strings.EqualFold(part, pp[i]) {
				return false
			}
			continue
		}
		if part != pp[i] {
			return false
		}
	}
	return true
}
