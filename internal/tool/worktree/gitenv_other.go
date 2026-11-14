//go:build !windows

package worktree

import (
	"context"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/internal/fileutil"
)

const lineEnd = "\n"

func envKey(k string) string { return k }

func checkTools(Options) error { return nil }

func (m *Manager) selectGit(_ context.Context, env map[string]string) map[string]string {
	return env
}

func lookPath(pathValue, name string) string {
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && fileutil.Accessible(candidate, fileutil.AccessExec) {
			return candidate
		}
	}
	return ""
}

func normCase(s string) string { return s }

func boundedArgv(args []string) (string, []string) {
	return "nice", append([]string{"-n", "19", "git"}, args...)
}

func symlinkFallsBackToCopy() bool { return false }

func prepareUnlink(string) {}

func normalizeGitBashPath(p string) string { return p }
