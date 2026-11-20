//go:build !windows

package tools

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fpGitRepo(t *testing.T, path string) string {
	t.Helper()
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	twGit(t, filepath.Dir(path), "init", "--initial-branch=main", path)
	if err := os.Mkdir(filepath.Join(path, ".worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFootprintWarningSizesTheTreeOnlyWhenTheCountDoesNotDecide(t *testing.T) {
	tmp := t.TempDir()
	twHome(t, tmp)
	shimDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	duLog := filepath.Join(tmp, "du.log")
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> \"%s\"\necho \"6000\t$2\"\n", duLog)
	if err := os.WriteFile(filepath.Join(shimDir, "du"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	crowded := fpGitRepo(t, filepath.Join(tmp, "crowded"))
	for i := 0; i < 10; i++ {
		if err := os.Mkdir(filepath.Join(crowded, ".worktrees", fmt.Sprintf("lane-%d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sparse := fpGitRepo(t, filepath.Join(tmp, "sparse"))
	for i := 0; i < 2; i++ {
		if err := os.Mkdir(filepath.Join(sparse, ".worktrees", fmt.Sprintf("lane-%d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var captured bytes.Buffer
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(slog.NewTextHandler(&captured, &slog.HandlerOptions{Level: slog.LevelWarn})))
	warnWorktreeFootprint(crowded)
	if _, err := os.Stat(duLog); err == nil {
		t.Fatal("the count alone decides: no walk of .worktrees/")
	}
	if !strings.Contains(captured.String(), "holds 10 tree(s)") {
		t.Fatalf("warning missing: %s", captured.String())
	}
	captured.Reset()
	warnWorktreeFootprint(sparse)
	raw, err := os.ReadFile(duLog)
	if err != nil {
		t.Fatalf("du never ran for the sparse tree: %v", err)
	}
	if got := strings.Count(string(raw), "-sm"); got != 1 {
		t.Fatalf("du ran %d times: %s", got, raw)
	}
	if !strings.Contains(captured.String(), "holds 2 tree(s) (5.9GB)") {
		t.Fatalf("warning missing the size: %s", captured.String())
	}
}
