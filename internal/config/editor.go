package config

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Editor a person set in EDITOR or VISUAL, else the first of the usual ones found on this machine, empty when there is none
func Editor() string {
	for _, name := range []string{"EDITOR", "VISUAL"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	candidates := []string{"nano", "vim", "vi", "code", "notepad"}
	if runtime.GOOS == "windows" {
		candidates = []string{"notepad", "code", "vim", "vi", "nano"}
	}
	for _, candidate := range candidates {
		if _, err := exec.LookPath(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// Command that runs the editor on the file, the editor's words split so one set with flags, such as code -w, runs with them
func EditorCommand(ctx context.Context, editor, path string) *exec.Cmd {
	words := strings.Fields(editor)
	return exec.CommandContext(ctx, words[0], append(words[1:], path)...)
}
