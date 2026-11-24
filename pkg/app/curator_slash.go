package app

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runs the curator CLI on argv in the profile scope of env, its report, help and usage errors written to out, a failure as its error line
func RunCuratorSlash(ctx context.Context, env *tools.Env, argv []string, out io.Writer) {
	tree := commands.NewCLI()
	tree.Root.SetOut(out)
	tree.Root.SetErr(out)
	tree.Root.SetContext(tools.WithToolEnv(ctx, env))
	err := tree.Execute(append([]string{"curator"}, argv...))
	if err == nil {
		return
	}
	var ae *commands.ArgparseError
	var exit *commands.ExitError
	switch {
	case errors.As(err, &ae):
		fmt.Fprintln(out, ae.Stderr())
	case errors.As(err, &exit):
		if exit.Msg != "" {
			fmt.Fprintln(out, exit.Msg)
		}
	default:
		fmt.Fprintln(out, i18n.T(ctx, "cli.commands.curator.failed", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}))
	}
}
