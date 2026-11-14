package env

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/proc"
)

var windowsBashStubDirs = []string{"system32", "windowsapps"}

func windowsBashCandidates(onPath string) []string {
	programFiles := os.Getenv("ProgramFiles")
	if programFiles == "" {
		programFiles = `C:\Program Files`
	}
	var candidates []string
	if override := os.Getenv("ICEBATH_GIT_BASH_PATH"); override != "" {
		candidates = append(candidates, override)
	}
	roots := []string{filepath.Join(programFiles, "Git")}
	if x86 := os.Getenv("ProgramFiles(x86)"); x86 != "" {
		roots = append(roots, filepath.Join(x86, "Git"))
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		roots = append(roots, filepath.Join(local, "icebath", "git"), filepath.Join(local, "Programs", "Git"))
	}
	for _, root := range roots {
		candidates = append(candidates, filepath.Join(root, "bin", "bash.exe"), filepath.Join(root, "usr", "bin", "bash.exe"))
	}
	if onPath != "" {
		norm := strings.ToLower(filepath.Clean(onPath))
		stub := false
		for _, s := range windowsBashStubDirs {
			if strings.Contains(norm, s) {
				stub = true
				break
			}
		}
		if !stub {
			candidates = append(candidates, onPath)
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range candidates {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

func bashStarts(ctx context.Context, candidate string) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.Command(candidate, "-c", "exit 0")
	proc.Hide(cmd)
	res, err := proc.Run(ctx, cmd)
	return err == nil && res.Exit.Success()
}

func findBash(ctx context.Context, tc Toolchain) string {
	staged, err := tc.StagedBash(ctx)
	if err != nil {
		slog.Debug("staged git bash unavailable", "err", err)
		staged = ""
	}
	if staged != "" && bashStarts(ctx, staged) {
		return staged
	}
	onPath, _ := exec.LookPath("bash")
	for _, c := range windowsBashCandidates(onPath) {
		if isFile(c) && bashStarts(ctx, c) {
			return c
		}
	}
	return ""
}

func (h *Host) FindShell(ctx context.Context) (string, error) {
	return h.FindBash(ctx)
}
