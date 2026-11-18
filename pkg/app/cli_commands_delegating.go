package app

import (
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pysys"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/spf13/cobra"
)

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
			fmt.Fprintln(pysys.Stderr, ae.Stderr())
		}
		return
	}
	if runErr != nil {
		printLines(c.commandsT("curator.failed", map[string]any{"error": pyrepr.ExceptionMessage(runErr)}))
	}
}
