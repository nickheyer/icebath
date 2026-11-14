//go:build !windows

package checkpoint

import (
	"context"
	"path/filepath"

	"github.com/nickheyer/icebath/internal/fileutil"
)

func envKey(k string) string { return k }

func (m *Manager) selectGit(_ context.Context, env map[string]string) (map[string]string, error) {
	return env, nil
}

func lookPath(pathValue, name string) string {
	for _, dir := range splitSearchPath(pathValue) {
		candidate := filepath.Join(dir, name)
		if isExecutableFile(candidate) && fileutil.Accessible(candidate, fileutil.AccessExec) {
			return candidate
		}
	}
	return ""
}
