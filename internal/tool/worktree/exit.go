package worktree

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"
)

type ExitChoice string

const (
	ChoiceKeep    ExitChoice = "keep"
	ChoiceArchive ExitChoice = "archive"
	ChoiceDiscard ExitChoice = "discard"
)

type ExitOption struct {
	Choice ExitChoice
	Label  Message
}

type ExitQuestion struct {
	State   string
	Title   Message
	Options []ExitOption
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func (m *Manager) WorktreeState(ctx context.Context, t *Tree) (string, error) {
	if t == nil || t.Path == "" || !isDir(t.Path) {
		return "", nil
	}
	dirty, err := m.isDirty(ctx, t.Path, t.RepoRoot, 10*time.Second)
	if err != nil {
		return "", err
	}
	if dirty {
		return "dirty", nil
	}
	unpushed, err := m.hasUnpushedCommits(ctx, t.Path, 10*time.Second)
	if err != nil {
		return "", err
	}
	if unpushed {
		return "unpushed", nil
	}
	return "", nil
}

func (m *Manager) ExitQuestion(ctx context.Context, t *Tree) (*ExitQuestion, error) {
	if t == nil {
		return nil, nil
	}
	state, err := m.WorktreeState(ctx, t)
	if err != nil || state == "" {
		return nil, err
	}
	busy, err := m.StillInUse(ctx, t.Path)
	if err != nil || busy {
		return nil, err
	}
	return &ExitQuestion{
		State: state,
		Title: Message{Key: "cli.worktree.exit_question_" + state, Args: map[string]string{"path": t.Path}},
		Options: []ExitOption{
			{Choice: ChoiceKeep, Label: Message{Key: "cli.worktree.exit_choice_keep"}},
			{Choice: ChoiceArchive, Label: Message{Key: "cli.worktree.exit_choice_archive"}},
			{Choice: ChoiceDiscard, Label: Message{Key: "cli.worktree.exit_choice_discard"}},
		},
	}, nil
}

func (m *Manager) ApplyExitChoice(ctx context.Context, t *Tree, choice ExitChoice, echo func(Message)) (bool, error) {
	if t == nil || (choice != ChoiceArchive && choice != ChoiceDiscard) {
		return false, nil
	}
	if choice == ChoiceArchive {
		echo(Message{Key: "cli.worktree.archiving", Args: map[string]string{"path": t.Path}})
		settings, err := m.Settings(ctx)
		if err != nil {
			return false, err
		}
		res, err := m.ArchiveThenRemove(ctx, t.RepoRoot, t.Path, ArchiveRemoveOptions{
			Reason: "session exit", MaxUntrackedMB: settings.ArchiveMaxFileMB, Unlock: true, ExcludePIDs: []int{os.Getpid()},
		})
		if err != nil {
			var missing *MissingInterfaceError
			if errors.As(err, &missing) {
				return false, err
			}
			text := err.Error()
			if ctx.Err() != nil {
				text = "interrupted"
			}
			echo(Message{Key: "cli.worktree.archive_failed", Args: map[string]string{"path": t.Path, "error": text}, Tone: ToneError})
			return true, nil
		}
		name := res.Ref[strings.LastIndex(res.Ref, "/")+1:]
		if res.Removed {
			echo(Message{Key: "cli.worktree.archived", Args: map[string]string{"path": t.Path, "ref": res.Ref}, Tone: ToneSuccess})
		} else {
			why := res.WhyKept
			if why == "" {
				why = "?"
			}
			echo(Message{Key: "cli.worktree.archived_kept", Args: map[string]string{"path": t.Path, "ref": res.Ref, "reason": why}, Tone: ToneWarning})
		}
		echo(Message{Key: "cli.worktree.restore_hint", Args: map[string]string{"name": name}, Indent: true})
		return true, nil
	}
	if err := m.releaseLSP(ctx, t.Path); err != nil {
		return false, err
	}
	if err := m.gitQuiet(ctx, t.RepoRoot, 10*time.Second, nil, "git worktree unlock failed (non-fatal)", "worktree", "unlock", t.Path); err != nil {
		return false, err
	}
	if err := m.gitQuiet(ctx, t.RepoRoot, 60*time.Second, nil, "Failed to remove worktree", "worktree", "remove", t.Path, "--force"); err != nil {
		return false, err
	}
	if t.Branch != "" && !isDir(t.Path) {
		if err := m.gitQuiet(ctx, t.RepoRoot, 10*time.Second, m.noninteractiveEnv(ctx), "Failed to delete branch "+t.Branch, "branch", "-D", t.Branch); err != nil {
			return false, err
		}
	}
	echo(Message{Key: "cli.worktree.discarded", Args: map[string]string{"path": t.Path}, Tone: ToneWarning})
	return true, nil
}

func (m *Manager) ExitCleanup(ctx context.Context, t *Tree, echo func(Message)) error {
	if t == nil {
		return nil
	}
	if _, err := os.Stat(t.Path); err != nil {
		return nil
	}
	path := map[string]string{"path": t.Path}
	dirty, err := m.isDirty(ctx, t.Path, t.RepoRoot, 10*time.Second)
	if err != nil {
		return err
	}
	if dirty {
		echo(Message{Key: "cli.worktree.uncommitted_keeping", Args: path, Tone: ToneWarning, Gap: true})
		return nil
	}
	unpushed, err := m.hasUnpushedCommits(ctx, t.Path, 10*time.Second)
	if err != nil {
		return err
	}
	if unpushed {
		shallow, err := m.repoIsShallow(ctx, t.RepoRoot, 5*time.Second)
		if err != nil {
			return err
		}
		if shallow {
			echo(Message{Key: "cli.worktree.shallow_clone_keeping", Args: path, Tone: ToneWarning, Gap: true})
			echo(Message{Key: "cli.worktree.next_session_deepens", Indent: true})
		} else {
			echo(Message{Key: "cli.worktree.unpushed_keeping", Args: path, Tone: ToneWarning, Gap: true})
			echo(Message{Key: "cli.worktree.clean_up_manually", Args: path, Indent: true})
		}
		return nil
	}
	blocked, err := m.RemovalBlocked(ctx, t.Path)
	if err != nil {
		return err
	}
	if blocked != nil {
		echo(Message{Key: "cli.worktree.kept_reason", Args: map[string]string{"path": t.Path, "reason": blocked.Reason}, Tone: ToneWarning, Gap: true})
		return nil
	}
	if err := m.releaseLSP(ctx, t.Path); err != nil {
		return err
	}
	if err := m.gitQuiet(ctx, t.RepoRoot, 10*time.Second, nil, "git worktree unlock failed (non-fatal)", "worktree", "unlock", t.Path); err != nil {
		return err
	}
	if err := m.gitQuiet(ctx, t.RepoRoot, 15*time.Second, nil, "Failed to remove worktree", "worktree", "remove", t.Path, "--force"); err != nil {
		return err
	}
	if err := m.gitQuiet(ctx, t.RepoRoot, 10*time.Second, nil, "Failed to delete branch "+t.Branch, "branch", "-D", t.Branch); err != nil {
		return err
	}
	echo(Message{Key: "cli.worktree.cleaned_up", Args: path, Tone: ToneSuccess})
	return nil
}
