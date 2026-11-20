package tools

import (
	"context"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
)

func TestExitQuestionNeverPromptsWithoutATty(t *testing.T) {
	repo := waRepo(t)
	wt := waTree(t, repo, "icebath-exit", true, false)
	prevRadio, prevTTY := worktreeExitRadiolist, worktreeExitHasTTY
	t.Cleanup(func() { worktreeExitRadiolist, worktreeExitHasTTY = prevRadio, prevTTY })
	worktreeExitHasTTY = func() bool { return false }
	worktreeExitRadiolist = func(ctx context.Context, title string, items []cliui.RadioItem, selected int, opts cliui.RadiolistOptions) (int, error) {
		t.Fatal("non-interactive exit must never prompt")
		return 0, nil
	}
	info := map[string]any{"path": wt, "repo_root": repo, "branch": "wt/icebath-exit"}
	if got := AskExitChoice(context.Background(), info); got != WorktreeExitKeep {
		t.Fatalf("choice %q", got)
	}
	if _, ok := info["exit_choice"]; ok {
		t.Fatalf("exit_choice recorded: %v", info)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("tree gone: %v", err)
	}
}
