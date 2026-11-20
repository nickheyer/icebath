package commands

import (
	"context"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/spf13/cobra"
)

// Binds the memory setup, status, off and reset commands plus the curator and insights trees
func (c *CLI) bindMemory() {
	for _, sub := range []string{"", "setup", "status"} {
		path := "memory"
		if sub != "" {
			path += " " + sub
		}
		c.Bind(path, func(cmd *cobra.Command, args []string) error { return runMemorySetup(cmd, sub, args) })
	}
	c.Bind("memory off", func(cmd *cobra.Command, args []string) error {
		return memory.MemoryOff(cmdContext(cmd), cmd.OutOrStdout())
	})
	c.Bind("memory reset", func(cmd *cobra.Command, args []string) error {
		target, _ := cmd.Flags().GetString("target")
		yes, _ := cmd.Flags().GetBool("yes")
		return memory.MemoryReset(cmdContext(cmd), cmd.OutOrStdout(), cmd.InOrStdin(), target, yes)
	})
	c.bindCurator()
	c.bindInsights()
}

// Command context, the background context when cobra carries none
func cmdContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// Runs one memory setup command line, a nonzero status becoming an ExitError
func runMemorySetup(cmd *cobra.Command, sub string, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	env, err := mcpCLIEnv(ctx)
	if err != nil {
		return err
	}
	provider := ""
	if sub == "setup" && len(args) > 0 {
		provider = args[0]
	}
	setup := memory.NewMemorySetup(env, &cliui.Console{In: cmd.InOrStdin(), Out: cmd.OutOrStdout()}, cmd.ErrOrStderr())
	code, err := setup.Run(ctx, sub, provider)
	if err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}
