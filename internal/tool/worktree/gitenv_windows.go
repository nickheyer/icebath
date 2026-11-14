//go:build windows

package worktree

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/internal/toolchain"
)

const lineEnd = "\r\n"

func envKey(k string) string { return strings.ToUpper(k) }

func checkTools(o Options) error {
	if o.Tools == nil {
		return &MissingInterfaceError{Name: "toolchain.Store"}
	}
	return nil
}

func (m *Manager) selectGit(ctx context.Context, env map[string]string) map[string]string {
	if _, err := m.tools.Ensure(ctx, toolchain.Git, toolchain.EnsureOptions{}); err != nil {
		slog.Debug("pinned git unavailable, using the system git", "error", err)
		return env
	}
	list, err := m.tools.Environ(ctx, environList(env), toolchain.Git)
	if err != nil {
		slog.Debug("pinned git environment unavailable, using the system git", "error", err)
		return env
	}
	return environMap(list)
}

func lookPath(pathValue, name string) string {
	pathext := os.Getenv("PATHEXT")
	if pathext == "" {
		pathext = ".COM;.EXE;.BAT;.CMD;.VBS;.JS;.WS;.MSC"
	}
	var exts []string
	for _, ext := range strings.Split(pathext, ";") {
		if ext != "" {
			exts = append(exts, strings.ToLower(ext))
		}
	}
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" {
			continue
		}
		for _, ext := range exts {
			candidate := filepath.Join(dir, name+ext)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate
			}
		}
	}
	return ""
}

func normCase(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "/", `\`))
}

func boundedArgv(args []string) (string, []string) {
	return "git", args
}

func symlinkFallsBackToCopy() bool { return true }

func prepareUnlink(path string) {
	if err := os.Chmod(path, 0o666); err != nil {
		slog.Debug("could not clear the read-only bit", "path", path, "error", err)
	}
}

var gitBashPath = regexp.MustCompile(`^/(?:(?:cygdrive|mnt)/)?([a-zA-Z])/(.*)$`)

func normalizeGitBashPath(p string) string {
	if p == "" {
		return p
	}
	if m := gitBashPath.FindStringSubmatch(p); m != nil {
		return strings.ToUpper(m[1]) + `:\` + strings.ReplaceAll(m[2], "/", `\`)
	}
	return p
}
