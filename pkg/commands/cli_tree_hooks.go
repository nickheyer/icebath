package commands

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/spf13/cobra"
)

// Binds icebath hooks and its list, test, revoke and doctor subcommands to the plugins hooks command
func (c *CLI) bindHooks() {
	for _, sub := range []string{"", "list", "test", "revoke", "doctor"} {
		path := strings.TrimSpace("hooks " + sub)
		c.Bind(path, func(cmd *cobra.Command, args []string) error { return runHooks(cmd, sub, args) })
	}
}

// Parses one hooks command line into its action and arguments and runs it
func runHooks(cmd *cobra.Command, sub string, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	a := plugins.HooksArgs{Action: sub}
	switch sub {
	case "test":
		a.Event = args[0]
		a.ForTool, _ = cmd.Flags().GetString("for-tool")
		a.PayloadFile, _ = cmd.Flags().GetString("payload-file")
	case "revoke":
		a.Command = args[0]
	}
	return plugins.HooksCommand(ctx, a, cmd.OutOrStdout())
}
