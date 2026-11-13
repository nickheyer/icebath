package app

import (
	"bytes"
	"errors"
	"fmt"
	"os"
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
	output, err := kanban.RunSlash(c.ctx, rest)
	if err != nil {
		output = c.commandsT("kanban.failed", map[string]any{"error": pyrepr.ExceptionMessage(err)})
	}
	if output != "" {
		printLines(output)
	}
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

// Parses /kanban tokens on the icebath CLI tree, the namespace when an action parsed, else the parser text renamed to /kanban
func ParseKanbanSlash(tokens []string) kanban.SlashParse {
	tree := commands.NewCLI()
	var bufOut, bufErr bytes.Buffer
	tree.Root.SetOut(&bufOut)
	tree.Root.SetErr(&bufErr)
	var p kanban.SlashParse
	handler := func(cmd *cobra.Command, posArgs []string) error {
		p.Parsed = true
		p.Namespace = tree.Namespace(cmd, posArgs)
		return nil
	}
	for _, path := range tree.Unbound() {
		if path == "kanban" || strings.HasPrefix(path, "kanban ") {
			tree.Bind(path, handler)
		}
	}
	execErr := tree.Execute(append([]string{"kanban"}, tokens...))
	if p.Parsed {
		p.Out, p.Err = bufOut.String(), bufErr.String()
		return p
	}
	p.Out, p.Err = kanbanSlashRename(bufOut.String(), tree.Root), kanbanSlashRename(bufErr.String(), tree.Root)
	var ae *commands.ArgparseError
	if errors.As(execErr, &ae) {
		p.ArgError, p.Usage = ae.Msg, kanbanUsageForError(tree.Root, tokens)
	}
	p.Failure = execErr
	return p
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
