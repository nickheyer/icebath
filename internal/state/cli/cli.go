package cli

import (
	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/state"
)

type commands struct {
	state *state.State
}

func Register(host *cli.CLI, st *state.State) {
	host.Register(commands{state: st})
}

func (c commands) Commands(*connect.Client) []*cobra.Command {
	return []*cobra.Command{
		{
			Use:   "connect <url>",
			Short: "Talk to the server at <url> from now on",
			Args:  cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				return c.state.SetBackend(args[0])
			},
		},
		{
			Use:   "disconnect",
			Short: "Go back to the server inside this process",
			Args:  cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error {
				return c.state.SetBackend("")
			},
		},
	}
}
