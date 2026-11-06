package commands

import (
	"context"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/spf13/cobra"
)

// Binds icebath memory, memory setup and memory status to the memory provider setup flow
func (c *CLI) bindMemory() {
	for _, sub := range []string{"", "setup", "status"} {
		path := "memory"
		if sub != "" {
			path += " " + sub
		}
		c.Bind(path, func(cmd *cobra.Command, args []string) error { return runMemorySetup(cmd, sub, args) })
	}
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
