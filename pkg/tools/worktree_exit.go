package tools

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/i18n"
)

const (
	WorktreeExitKeep    = "keep"
	WorktreeExitArchive = "archive"
	WorktreeExitDiscard = "discard"
)

// Radiolist the exit question draws, the curses_ui attribute hermes tests patch
var worktreeExitRadiolist = func(ctx context.Context, title string, items []cliui.RadioItem, selected int, opts cliui.RadiolistOptions) (int, error) {
	return (&cliui.Console{In: os.Stdin, Out: os.Stdout}).CursesRadiolist(ctx, title, items, selected, opts)
}

// Whether stdin and stdout are both terminals, swapped by tests standing for a captured stdin
var worktreeExitHasTTY = func() bool {
	for _, f := range []*os.File{os.Stdin, os.Stdout} {
		if f == nil || !(isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())) {
			return false
		}
	}
	return true
}

// Dirty, unpushed or empty for a clean and pushed or gone tree
func WorktreeState(info map[string]any) string {
	path, _ := info["path"].(string)
	repoRoot, _ := info["repo_root"].(string)
	if path == "" {
		return ""
	}
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		return ""
	}
	if worktreeIsDirty(path, repoRoot, 10*time.Second) {
		return "dirty"
	}
	if worktreeHasUnpushedCommits(path, 10*time.Second) {
		return "unpushed"
	}
	return ""
}

// Whether another process still works in the tree once the grace has passed
func worktreeStillInUse(path string, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	for len(LiveProcessPids([]string{path}, []int{os.Getpid()})[path]) > 0 {
		if !time.Now().Before(deadline) {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// Asks what to do with a dirty or unpushed tree and records the answer as exit_choice, keep on every skip
func AskExitChoice(ctx context.Context, info map[string]any) string {
	if len(info) == 0 || !worktreeExitHasTTY() {
		return WorktreeExitKeep
	}
	state := WorktreeState(info)
	if state == "" {
		return WorktreeExitKeep
	}
	path, _ := info["path"].(string)
	if worktreeStillInUse(path, 5*time.Second) {
		return WorktreeExitKeep
	}
	title := i18n.T(ctx, "cli.worktree.exit_question_"+state, "", map[string]any{"path": path})
	items := []cliui.RadioItem{
		cliui.PlainItem(i18n.T(ctx, "cli.worktree.exit_choice_keep", "", nil)),
		cliui.PlainItem(i18n.T(ctx, "cli.worktree.exit_choice_archive", "", nil)),
		cliui.PlainItem(i18n.T(ctx, "cli.worktree.exit_choice_discard", "", nil)),
	}
	zero := 0
	choice := WorktreeExitKeep
	idx, err := worktreeExitRadiolist(ctx, title, items, 0, cliui.RadiolistOptions{CancelReturns: &zero})
	if err != nil {
		toolsLogger(nil).Debug("worktree exit question failed; keeping the tree", "error", err.Error())
	} else if idx == 1 {
		choice = WorktreeExitArchive
	} else if idx == 2 {
		choice = WorktreeExitDiscard
	}
	info["exit_choice"] = choice
	return choice
}

// Carries out an archive or discard answer once, true when the tree was handled or a message printed
func ApplyExitChoice(ctx context.Context, info map[string]any, echo func(string)) bool {
	choice, _ := info["exit_choice"].(string)
	delete(info, "exit_choice")
	if choice != WorktreeExitArchive && choice != WorktreeExitDiscard {
		return false
	}
	path, _ := info["path"].(string)
	repoRoot, _ := info["repo_root"].(string)
	branch, _ := info["branch"].(string)
	if choice == WorktreeExitArchive {
		echo(i18n.T(ctx, "cli.worktree.archiving", "", map[string]any{"path": path}))
		res, err := ArchiveThenRemove(repoRoot, path, "session exit", SweepSettings().ArchiveMaxFileMB, true, nil, false, []int{os.Getpid()})
		if err != nil {
			msg := err.Error()
			if msg == "" {
				msg = "interrupted"
			}
			echo("\x1b[31m" + i18n.T(ctx, "cli.worktree.archive_failed", "", map[string]any{"path": path, "error": msg}) + "\x1b[0m")
			return true
		}
		name := res.Ref[strings.LastIndex(res.Ref, "/")+1:]
		if res.Removed {
			echo("\x1b[32m" + i18n.T(ctx, "cli.worktree.archived", "", map[string]any{"path": path, "ref": res.Ref}) + "\x1b[0m")
		} else {
			why := res.WhyKept
			if why == "" {
				why = "?"
			}
			echo("\x1b[33m" + i18n.T(ctx, "cli.worktree.archived_kept", "", map[string]any{"path": path, "ref": res.Ref, "reason": why}) + "\x1b[0m")
		}
		echo("  " + i18n.T(ctx, "cli.worktree.restore_hint", "", map[string]any{"name": name}))
		return true
	}
	releaseLspClients(path)
	worktreeGitQuiet([]string{"worktree", "unlock", path}, repoRoot, 10*time.Second, "git worktree unlock failed (non-fatal)", nil)
	worktreeGitQuiet([]string{"worktree", "remove", path, "--force"}, repoRoot, 60*time.Second, "Failed to remove worktree", nil)
	if branch != "" {
		if st, err := os.Stat(path); err != nil || !st.IsDir() {
			worktreeGitQuiet([]string{"branch", "-D", branch}, repoRoot, 10*time.Second, "Failed to delete branch "+branch, NoninteractiveGitEnv(nil))
		}
	}
	echo("\x1b[33m" + i18n.T(ctx, "cli.worktree.discarded", "", map[string]any{"path": path}) + "\x1b[0m")
	return true
}
