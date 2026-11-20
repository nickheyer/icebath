package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runs the curator CLI on argv with its report, help and usage errors written to out, a failure as its error line
func RunCuratorSlash(ctx context.Context, env *tools.Env, argv []string, out io.Writer) {
	tree := commands.NewCLI()
	tree.Root.SetOut(out)
	tree.Root.SetErr(out)
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
		_, runErr = memory.CuratorCommand(ctx, env, ns, out)
		return nil
	}
	for _, path := range tree.Unbound() {
		if path == "curator" || strings.HasPrefix(path, "curator ") {
			tree.Bind(path, handler)
		}
	}
	execErr := tree.Execute(append([]string{"curator"}, argv...))
	if !ran {
		var ae *commands.ArgparseError
		if errors.As(execErr, &ae) {
			fmt.Fprintln(out, ae.Stderr())
		}
		return
	}
	if runErr != nil {
		fmt.Fprintln(out, i18n.T(ctx, "cli.commands.curator.failed", "", map[string]any{"error": pyrepr.ExceptionMessage(runErr)}))
	}
}
