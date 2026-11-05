package commands

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/spf13/cobra"
)

// Binds icebath moa and its list, configure and delete paths to the provider MoA command
func (c *CLI) bindMoA() {
	for _, path := range []string{"moa", "moa list", "moa configure", "moa delete"} {
		bare := path == "moa"
		c.Bind(path, func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			a := provider.MoAArgs{}
			if !bare {
				a.Command = cmd.CalledAs()
			}
			if len(args) > 0 {
				a.Name = args[0]
			}
			err := provider.CmdMoA(ctx, cmd.OutOrStdout(), a)
			var exit *provider.MoAExitError
			if errors.As(err, &exit) {
				return &ExitError{Code: 1, Msg: exit.Message}
			}
			return err
		})
	}
}
