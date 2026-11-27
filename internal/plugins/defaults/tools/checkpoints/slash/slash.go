package slash

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/internal/checkpoints"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/checkpoints/server"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/slash"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/strs"
)

type commands struct {
	config   *config.Config
	sessions *sessions.Registry
	terminal *terminal.Terminal
}

func Register(registry *slash.Registry, cfg *config.Config, stores *sessions.Registry, host *terminal.Terminal) {
	registry.Register(commands{config: cfg, sessions: stores, terminal: host})
}

// Usage of /rollback, as its help and its refusals name it
const usage = "[list | diff [N] | <N> [file] [--all]]"

func (c commands) Commands() []slash.Command {
	return []slash.Command{{Name: "rollback", Usage: usage, Help: "Restore files from a checkpoint",
		Run: c.rollback}}
}

// Lists the checkpoints of the session's project, shows what changed since one, or restores the project or one file of it to one: the
// files the user changed since kept unless --all is given
func (c commands) rollback(ctx context.Context, call *slash.Call) (string, error) {
	scope, err := server.Scope(ctx, c.config, c.sessions, c.terminal, call.SessionID)
	if err != nil {
		return "", err
	}
	if !scope.Enabled() {
		return checkpoints.NotEnabled, nil
	}
	all := false
	var words []string
	for _, word := range strings.Fields(call.Argument) {
		if lower := strings.ToLower(word); lower == "--all" || lower == "--force" {
			all = true
			continue
		}
		words = append(words, word)
	}
	first := ""
	if len(words) > 0 {
		first = strings.ToLower(words[0])
	}
	switch first {
	case "", "list", "ls":
		listing := list(scope.List(scope.Directory), scope.Directory)
		if scope.Refusal != "" {
			return scope.Refusal + "\n" + listing, nil
		}
		return listing, nil
	case "diff":
		reference := ""
		if len(words) > 1 {
			reference = words[1]
		}
		return c.diff(scope, reference)
	}
	if scope.Refusal != "" {
		return scope.Refusal, nil
	}
	entries := scope.List(scope.Directory)
	if len(entries) == 0 {
		return none(scope.Directory), nil
	}
	sha, err := scope.Resolve(scope.Directory, words[0])
	if err != nil {
		return "", err
	}
	file := strings.Join(words[1:], " ")
	restored, err := scope.Restore(scope.Directory, sha, file, file == "" && !all)
	if err != nil {
		return "", err
	}
	return strings.Join(server.Report(server.Restored(restored)), "\n"), nil
}

// What changed in the project since the checkpoint, or since the oldest kept for none, its stat and a diff cut to a chat message
func (c commands) diff(scope *checkpoints.Scope, reference string) (string, error) {
	if scope.Refusal != "" {
		return scope.Refusal, nil
	}
	diff, sha, err := server.Diff(scope, reference)
	if err != nil {
		return "", err
	}
	if diff.Empty && sha == "" {
		return none(scope.Directory), nil
	}
	if diff.Empty {
		return "No changes since checkpoint " + sha[:min(8, len(sha))] + ".", nil
	}
	var parts []string
	if diff.Stat != "" {
		parts = append(parts, "```\n"+diff.Stat+"\n```")
	}
	if diff.Patch != "" {
		lines := strings.Split(diff.Patch, "\n")
		body, cut := diff.Patch, false
		if len(lines) > 60 {
			body, cut = strings.Join(lines[:60], "\n"), true
		}
		if runes := []rune(body); len(runes) > 3000 {
			body, cut = string(runes[:3000]), true
		}
		if cut {
			body += "\n…"
			parts = append(parts, "```diff\n"+body+"\n```", "The diff is cut here. It has "+strs.Count(len(lines), "line")+
				" in all, and /rollback diff in icebath in your terminal shows all of it.")
			return strings.Join(parts, "\n\n"), nil
		}
		parts = append(parts, "```diff\n"+body+"\n```")
	}
	return strings.Join(parts, "\n\n"), nil
}

// What /rollback says when the folder has no checkpoint yet
func none(directory string) string {
	return "There are no checkpoints of " + directory + " yet. One is saved each time the agent is about to change files there."
}

// Numbered listing of checkpoints with how to use them
func list(entries []checkpoints.Entry, directory string) string {
	if len(entries) == 0 {
		return none(directory)
	}
	lines := []string{"Checkpoints of " + directory + ":\n"}
	for i, entry := range entries {
		when := entry.When.Local().Format("2006-01-02 15:04")
		stat := ""
		if entry.Files != 0 {
			stat = "  " + strs.Count(entry.Files, "file") + " changed, " + strs.Count(entry.Insertions, "line") + " added, " +
				strs.Number(float64(entry.Deletions)) + " removed"
		}
		lines = append(lines, fmt.Sprintf("  %d. %s  %s  %s%s", i+1, entry.Short, when, entry.Reason, stat))
	}
	lines = append(lines,
		"\n  /rollback <N>             Restore your files to checkpoint N",
		"  /rollback diff <N>        Show what changed since checkpoint N",
		"  /rollback diff            Show everything that changed since the oldest checkpoint",
		"  /rollback <N> <file>      Restore one file from checkpoint N")
	return strings.Join(lines, "\n")
}
