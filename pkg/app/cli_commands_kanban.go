//go:build !icebath_core && !icebath_noextras

package app

import (
	"bytes"
	"errors"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/spf13/cobra"
)

// Adds the CLI tree parser to the kanban slash runner
func init() {
	kanban.ParseSlash = ParseKanbanSlash
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
