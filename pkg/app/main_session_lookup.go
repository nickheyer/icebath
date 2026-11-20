package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Git repo root of the working directory, else the directory itself, empty when neither resolves
func resolveWorkspaceKey() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if text := strings.TrimSpace(strings.ToValidUTF8(string(out), "�")); err == nil && text != "" {
		if abs, aerr := filepath.Abs(text); aerr == nil {
			return abs
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}
