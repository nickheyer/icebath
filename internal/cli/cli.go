package cli

import (
	"context"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"
)

type Registrant interface {
	Commands(conn *connect.Client) []*cobra.Command
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Commands(conn *connect.Client) []*cobra.Command {
	var commands []*cobra.Command
	for _, registrant := range r.registrants {
		commands = append(commands, registrant.Commands(conn)...)
	}
	return commands
}

type CLI struct {
	Registry
}

func New() *CLI {
	return &CLI{}
}

func (c *CLI) Run(ctx context.Context, conn *connect.Client, args []string) error {
	root := &cobra.Command{
		Use:   "icebath",
		Short: "A tiling agentic harness",
		Long: `A tiling agentic harness.

  icebath             open the terminal app
  icebath server      run a backend the app and CLI can connect to
  icebath <command>   run one command and exit`,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.AddCommand(c.Commands(conn)...)
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}
