package main

import (
	"context"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/mcpserve"
	"github.com/spf13/cobra"
)

// Runs icebath mcp-serve with its verbose, tools and accept-hooks flags
func mcpServeGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	f := cmd.Flags()
	verbose, _ := f.GetBool("verbose")
	withTools, _ := f.GetBool("tools")
	acceptHooks, _ := f.GetBool("accept-hooks")
	return mcpserve.RunServer(ctx, verbose, withTools, acceptHooks), nil
}
