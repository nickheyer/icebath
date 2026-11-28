package server

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/checkpoints"
	"github.com/nickheyer/icebath/internal/git"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Longest one git step of a diff may take, twice that for the diff itself
	diffTimeout = 15 * time.Second
	// Untracked files rendered as new files, and those listed by name
	untrackedDiffs  = 50
	untrackedListed = 15
	// Lines and characters of a diff shown in the chat
	diffLines = 60
	diffChars = 3000
)

// Words /diff takes for what it compares, the working tree against the index when none is given
var diffModes = map[string]string{"staged": "staged", "--staged": "staged", "cached": "staged", "--cached": "staged", "all": "all", "--all": "all",
	"head": "all", "session": "session"}

// What a diff found: its stat, its patch, the files git does not track, or why it could not be read
type workingDiff struct {
	stat, patch string
	untracked   []string
	failure     string
}

// Runs git in the folder with an environment that never prompts, pages or runs hooks, diffs never through drivers or text conversion
func diffGit(ctx context.Context, dir string, env map[string]string, timeout time.Duration, args ...string) (*git.Result, error) {
	if len(args) > 0 && args[0] == "diff" {
		args = slices.Insert(slices.Clone(args), 1, "--no-ext-diff", "--no-textconv")
	}
	return git.Run(ctx, dir, env, timeout, nil, append([]string{"-c", "core.quotePath=false"}, args...)...)
}

// Why git could not run, as the user reads it
func gitFailure(err error) string {
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return "git is not installed, or not on your PATH. Install git to use /diff."
	}
	if _, ok := errors.AsType[*git.TimeoutError](err); ok {
		return "git took too long to list the changes. Try /diff --stat, or name fewer paths."
	}
	return "git failed. " + strs.Full(err.Error())
}

// Changes of the git checkout the folder is in: of the working tree against the index, of the index against HEAD (staged), or of both
// (all); the paths given alone when there are any, else with the files git does not track rendered as new ones
func collect(ctx context.Context, dir, mode string, paths []string) workingDiff {
	env := git.Env(ctx, nil)
	inside, err := git.Run(ctx, dir, env, 5*time.Second, nil, "rev-parse", "--is-inside-work-tree")
	switch {
	case err != nil:
		return workingDiff{failure: gitFailure(err)}
	case inside.Code != 0:
		return workingDiff{failure: "This session's folder is not in a git repository, so there are no git changes to show."}
	}
	base := []string{"diff"}
	switch mode {
	case "staged":
		base = append(base, "--cached")
	case "all":
		base = append(base, "HEAD")
	}
	var pathspec []string
	if len(paths) > 0 {
		pathspec = append([]string{"--"}, paths...)
	}
	stat, err := diffGit(ctx, dir, env, diffTimeout, slices.Concat(base, []string{"--stat"}, pathspec)...)
	if err != nil {
		return workingDiff{failure: gitFailure(err)}
	}
	patch, err := diffGit(ctx, dir, env, 2*diffTimeout, slices.Concat(base, pathspec)...)
	if err != nil {
		return workingDiff{failure: gitFailure(err)}
	}
	out := workingDiff{stat: strings.TrimSpace(stat.Stdout), patch: strings.TrimSpace(patch.Stdout)}
	if mode == "staged" || len(paths) > 0 {
		return out
	}
	listed, err := git.Run(ctx, dir, env, diffTimeout, nil, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return workingDiff{failure: gitFailure(err)}
	}
	if listed.Code == 0 {
		for _, line := range strings.Split(listed.Stdout, "\n") {
			if line = strings.TrimSuffix(line, "\r"); strings.TrimSpace(line) != "" {
				out.untracked = append(out.untracked, line)
			}
		}
	}
	var chunks []string
	for _, file := range out.untracked[:min(len(out.untracked), untrackedDiffs)] {
		if added, err := diffGit(ctx, dir, env, diffTimeout, "diff", "--no-index", "--", os.DevNull, file); err == nil {
			if text := strings.TrimRight(added.Stdout, "\n"); strings.TrimSpace(text) != "" {
				chunks = append(chunks, text)
			}
		}
	}
	if len(out.untracked) > untrackedDiffs {
		chunks = append(chunks, "And "+strs.Count(len(out.untracked)-untrackedDiffs, "more new file")+" not shown")
	}
	if len(chunks) > 0 {
		out.patch = strings.TrimSpace(out.patch + "\n" + strings.Join(chunks, "\n"))
	}
	return out
}

// Shows the changes of the session's folder: its git changes in the mode the words name, or with session everything changed since the
// oldest checkpoint the session keeps; --stat for the summary alone, and paths to show only those
func (a *Agent) Diff(ctx context.Context, id, argument string) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	store, err := a.store()
	if err != nil {
		return "", err
	}
	session, err := store.Get(ctx, id)
	if err != nil {
		return "", err
	}
	mode, summary := "working", false
	var paths []string
	for words := strings.Fields(argument); len(words) > 0; {
		word := words[0]
		lower := strings.ToLower(word)
		switch words = words[1:]; {
		case word == "--":
			paths, words = append(paths, words...), nil
		case lower == "--stat" || lower == "stat":
			summary = true
		case diffModes[lower] != "" && len(paths) == 0:
			mode = diffModes[lower]
		default:
			paths = append(paths, word)
		}
	}
	var found workingDiff
	if mode == "session" {
		scope := checkpoints.For(a.settings(r), session.GetDirectory(), a.terminal)
		switch {
		case !scope.Enabled():
			return "", errors.New(checkpoints.NotEnabled + "\nPlain /diff still works, because it uses git.")
		case scope.Refusal != "":
			return "", errors.New(scope.Refusal)
		}
		diff, err := scope.SessionDiff(scope.Directory)
		if err != nil {
			return "", err
		}
		found = workingDiff{stat: diff.Stat, patch: diff.Patch}
	} else {
		found = collect(ctx, session.GetDirectory(), mode, paths)
	}
	if found.failure != "" {
		return "", errors.New(found.failure)
	}
	return rendered(found, summary), nil
}

// The stat, the untracked files and the patch cut to a chat message
func rendered(d workingDiff, summary bool) string {
	if d.stat == "" && d.patch == "" && len(d.untracked) == 0 {
		return "No changes to show."
	}
	var parts []string
	if d.stat != "" {
		parts = append(parts, "```\n"+d.stat+"\n```")
	}
	if len(d.untracked) > 0 {
		var lines []string
		for _, file := range d.untracked[:min(len(d.untracked), untrackedListed)] {
			lines = append(lines, "+ "+file)
		}
		more := ""
		if len(d.untracked) > untrackedListed {
			more = "\nAnd " + strs.Number(float64(len(d.untracked)-untrackedListed)) + " more"
		}
		parts = append(parts, "**New files that git does not track**\n```\n"+strings.Join(lines, "\n")+more+"\n```")
	}
	if !summary && d.patch != "" {
		lines := strings.Split(d.patch, "\n")
		body, cut := d.patch, len(lines) > diffLines
		if cut {
			body = strings.Join(lines[:diffLines], "\n")
		}
		if utf8.RuneCountInString(body) > diffChars {
			body, cut = string([]rune(body)[:diffChars]), true
		}
		note := ""
		if cut {
			note = "\nThe rest is cut, " + strs.Count(len(lines), "line") + " in all. Type /diff --stat for a summary."
		}
		parts = append(parts, "```diff\n"+body+note+"\n```")
	}
	return strings.Join(parts, "\n\n")
}
