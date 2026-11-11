package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/spf13/cobra"
)

// Handles /suggestions through the shared handler with the local platform as the CLI origin
func (c *CLI) handleSuggestionsCommand(cmd string) {
	args := strings.Join(shlexArgs(cmd), " ")
	output, err := cron.HandleSuggestionsCommand(c.ctx, args, nil, "cli")
	if err != nil {
		output = c.commandsT("suggestions.failed", map[string]any{"error": pyrepr.ExceptionMessage(err)})
	}
	c.consolePrint(output)
}

// Handles /curator through the icebath curator parser, status when bare, argparse exits never ending the session
func (c *CLI) handleCuratorCommand(cmd string) {
	var tokens []string
	if cmd != "" {
		parts, err := pyshlex.SplitPOSIX(cmd, false)
		if err != nil {
			panic(err)
		}
		if len(parts) > 1 {
			tokens = parts[1:]
		}
	}
	if len(tokens) == 0 {
		tokens = []string{"status"}
	}
	tree := commands.NewCLI()
	var runErr error
	ran := false
	handler := func(cmd *cobra.Command, posArgs []string) error {
		ran = true
		ns := tree.Namespace(cmd, posArgs)
		var sub any
		if words := strings.Fields(cmd.CommandPath()); len(words) > 2 {
			sub = words[2]
		}
		ns["curator_command"] = sub
		_, runErr = memory.CuratorCommand(c.ctx, c.env, ns, c.console.Out)
		return nil
	}
	for _, path := range tree.Unbound() {
		if path == "curator" || strings.HasPrefix(path, "curator ") {
			tree.Bind(path, handler)
		}
	}
	execErr := tree.Execute(append([]string{"curator"}, tokens...))
	if !ran {
		var ae *commands.ArgparseError
		if errors.As(execErr, &ae) {
			fmt.Fprintln(os.Stderr, ae.Stderr())
		}
		return
	}
	if runErr != nil {
		printLines(c.commandsT("curator.failed", map[string]any{"error": pyrepr.ExceptionMessage(runErr)}))
	}
}

// Handles /kanban, handing the text after the command word to the shared slash runner
func (c *CLI) handleKanbanCommand(cmd string) {
	rest := strings.TrimLeft(pystr.Strip(cmd), "/")
	if strings.HasPrefix(rest, "kanban") {
		rest = pystr.Lstrip(rest[len("kanban"):])
	}
	output, err := RunKanbanSlash(c.ctx, rest)
	if err != nil {
		output = c.commandsT("kanban.failed", map[string]any{"error": pyrepr.ExceptionMessage(err)})
	}
	if output != "" {
		printLines(output)
	}
}

const slashKanbanHelp = "**/kanban** — manage the shared task board.\n" +
	"\n" +
	"Common subcommands:\n" +
	"  `list` (alias `ls`)   List tasks on the current board\n" +
	"  `show <id>`           Task details + comments + events\n" +
	"  `stats`               Per-status / per-assignee counts\n" +
	"  `create <title>…`     Create a task (auto-subscribes you to events)\n" +
	"  `comment <id> <msg>`  Append a comment\n" +
	"  `attach <id> <path>`  Attach a local file; `attachments <id>` to list\n" +
	"  `complete <id>…`      Mark task(s) done\n" +
	"  `request-review <id>` Enter first-class review; `request-changes <id> <reason>` returns an active review to its implementer\n" +
	"  `block <id> [reason]` Mark blocked; `schedule <id> [reason]` parks time-delay work; `unblock <id>` to revive\n" +
	"  `assign <id> <profile>`  Reassign\n" +
	"  `boards list`         Show all boards\n" +
	"  `assignees`           Known profiles + counts\n" +
	"  `context <id>`        Full worker-context dump\n" +
	"  `runs <id>`           Attempt history\n" +
	"  `log <id>`            Worker log\n" +
	"\n" +
	"Run `/kanban <subcommand> -h` for arguments. " +
	"Read-only commands are safe while an agent is running."

// Slash tokens of a kanban invocation, non POSIX on Windows with one wrapping quote pair stripped
func kanbanSlashTokens(rest string) ([]string, error) {
	if rest == "" || pystr.Strip(rest) == "" {
		return nil, nil
	}
	var raw []string
	var err error
	if runtime.GOOS == "windows" {
		raw, err = pyshlex.SplitNonPOSIX(rest)
	} else {
		raw, err = pyshlex.SplitPOSIX(rest, false)
	}
	if err != nil {
		return nil, err
	}
	tokens := make([]string, 0, len(raw))
	for _, tok := range raw {
		if len(tok) >= 2 && tok[0] == tok[len(tok)-1] && (tok[0] == '\'' || tok[0] == '"') {
			tok = tok[1 : len(tok)-1]
		}
		tokens = append(tokens, tok)
	}
	return tokens, nil
}

// Prog argparse shows for a kanban parser, /kanban and its direct children renamed, deeper ones built under the wrapper
func kanbanSlashProg(commandPath string) string {
	words := strings.Fields(commandPath)
	switch {
	case len(words) <= 2:
		return "/kanban"
	case len(words) == 3:
		return "/kanban " + words[2]
	}
	return "/kanban-wrap " + strings.Join(words[1:], " ")
}

// Text with every icebath kanban parser path replaced by the prog the slash parser shows, longest first
func kanbanSlashRename(text string, root *cobra.Command) string {
	kb, _, err := root.Find([]string{"kanban"})
	if err != nil || kb == nil || kb.Name() != "kanban" {
		return text
	}
	var paths []string
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		paths = append(paths, cmd.CommandPath())
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
	}
	walk(kb)
	sort.SliceStable(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) })
	for _, path := range paths {
		text = strings.ReplaceAll(text, path, kanbanSlashProg(path))
	}
	return text
}

// Runs a /kanban string through the kanban parser and command, returning its captured stdout and stderr
func RunKanbanSlash(ctx context.Context, rest string) (string, error) {
	tokens, err := kanbanSlashTokens(rest)
	if err != nil {
		return "", err
	}
	if len(tokens) == 0 || tokens[0] == "help" || tokens[0] == "--help" || tokens[0] == "-h" || tokens[0] == "?" {
		return slashKanbanHelp, nil
	}
	tree := commands.NewCLI()
	var bufOut, bufErr bytes.Buffer
	tree.Root.SetOut(&bufOut)
	tree.Root.SetErr(&bufErr)
	ran := false
	handler := func(cmd *cobra.Command, posArgs []string) error {
		ran = true
		_, kerr := kanban.KanbanCommand(ctx, tree.Namespace(cmd, posArgs), &bufOut, &bufErr)
		var exit *commands.ExitError
		if kerr != nil && !errors.As(kerr, &exit) {
			fmt.Fprintf(&bufErr, "error: %s\n", pyrepr.ExceptionMessage(kerr))
		}
		return nil
	}
	for _, path := range tree.Unbound() {
		if path == "kanban" || strings.HasPrefix(path, "kanban ") {
			tree.Bind(path, handler)
		}
	}
	execErr := tree.Execute(append([]string{"kanban"}, tokens...))
	if !ran {
		out := pystr.Rstrip(kanbanSlashRename(bufOut.String(), tree.Root))
		var ae *commands.ArgparseError
		if errors.As(execErr, &ae) {
			return "⚠ /kanban usage error\n" + kanbanUsageForError(tree.Root, tokens) + "\n" + ae.Msg, nil
		}
		if execErr == nil && out != "" {
			return out, nil
		}
		body := pystr.Rstrip(kanbanSlashRename(bufErr.String(), tree.Root))
		if body == "" {
			body = out
		}
		if body == "" && execErr != nil {
			body = execErr.Error()
		}
		if body != "" {
			return "⚠ /kanban usage error\n" + body, nil
		}
		return "⚠ /kanban usage error", nil
	}
	out, errText := pystr.Rstrip(bufOut.String()), pystr.Rstrip(bufErr.String())
	if errText != "" && out != "" {
		return out + "\n" + errText, nil
	}
	if errText != "" {
		return errText, nil
	}
	if out == "" {
		return "(no output)", nil
	}
	return out, nil
}

// Usage line of the subcommand parser the first token names, else of the kanban parser
func kanbanUsageForError(root *cobra.Command, tokens []string) string {
	kb, _, err := root.Find([]string{"kanban"})
	if err != nil || kb == nil {
		return ""
	}
	target := kb
	if len(tokens) > 0 {
		for _, sub := range kb.Commands() {
			if sub.Name() == tokens[0] || sub.HasAlias(tokens[0]) {
				target = sub
				break
			}
		}
	}
	return pystr.Rstrip("usage: " + kanbanSlashRename(target.UseLine(), root))
}
