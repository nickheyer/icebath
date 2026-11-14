//go:build !windows

package env

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/nickheyer/icebath/internal/fileutil"
)

func executableFile(p string) bool {
	return p != "" && isFile(p) && fileutil.Accessible(p, fileutil.AccessExec)
}

func findBash(_ context.Context, _ Toolchain) string {
	if onPath, err := exec.LookPath("bash"); err == nil {
		return onPath
	}
	for _, candidate := range []string{"/usr/bin/bash", "/bin/bash", os.Getenv("SHELL"), "/bin/sh"} {
		if executableFile(candidate) {
			return candidate
		}
	}
	return ""
}

func (h *Host) FindShell(ctx context.Context) (string, error) {
	if user := os.Getenv("SHELL"); executableFile(user) && spawnCompatibleShells[filepath.Base(user)] {
		return user, nil
	}
	return h.FindBash(ctx)
}
