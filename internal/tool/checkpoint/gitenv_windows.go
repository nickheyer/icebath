//go:build windows

package checkpoint

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/internal/toolchain"
)

func envKey(k string) string { return strings.ToUpper(k) }

func (m *Manager) selectGit(ctx context.Context, env map[string]string) (map[string]string, error) {
	if _, err := m.tools.Ensure(ctx, toolchain.Git, toolchain.EnsureOptions{}); err != nil {
		slog.Debug("pinned git unavailable, using the system git", "error", err)
		return env, nil
	}
	list, err := m.tools.Environ(ctx, environList(env), toolchain.Git)
	if err != nil {
		slog.Debug("pinned git environment unavailable, using the system git", "error", err)
		return env, nil
	}
	return environMap(list), nil
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
	for _, dir := range splitSearchPath(pathValue) {
		for _, ext := range exts {
			candidate := filepath.Join(dir, name+ext)
			if isExecutableFile(candidate) {
				return candidate
			}
		}
	}
	return ""
}
