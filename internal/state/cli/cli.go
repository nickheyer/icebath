package cli

import (
	"net/url"

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
			Use:     "connect <url>",
			GroupID: cli.GroupApp,
			Short:   "Use the icebath server at this address from now on",
			Long: "Makes icebath on this computer use a server that you run with icebath server, such as one on another computer, instead of " +
				"its own. Put the server's password in the address, as the example shows. The address and password of a server are in " +
				"/settings under Server.",
			Example: "  icebath connect http://icebath:PASSWORD@192.168.1.20:7433",
			Args:    cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if err := c.state.SetBackend(args[0]); err != nil {
					return err
				}
				shown := args[0]
				if u, err := url.Parse(args[0]); err == nil {
					shown = u.Redacted()
				}
				cli.NewConsole(cmd).Success("icebath uses the server at " + shown + " from now on. Run icebath disconnect to go back to its own.")
				return nil
			},
		},
		{
			Use:     "disconnect",
			GroupID: cli.GroupApp,
			Short:   "Go back to the server built into icebath",
			Long:    "Stops using the server that connect set, so icebath runs its own server again.",
			Args:    cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				if err := c.state.SetBackend(""); err != nil {
					return err
				}
				cli.NewConsole(cmd).Success("icebath uses its own server again.")
				return nil
			},
		},
	}
}
