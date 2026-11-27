package cli

import (
	"cmp"
	"context"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/launch"
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
	// Flags of a launch of the app, which the root's help lists, and those taken ahead of the command, which it runs the work around it with
	launch []launch.Flag
	given  launch.Given
}

func New() *CLI {
	return &CLI{}
}

// Has the root's help list the flags a launch of the app takes, and hands the command those given ahead of it, by their first spelling
func (c *CLI) Launch(flags []launch.Flag, given launch.Given) {
	c.launch, c.given = flags, given
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
	for _, flag := range c.launch {
		long, short := "", ""
		for _, name := range flag.Names {
			switch {
			case strings.HasPrefix(name, "--"):
				long = cmp.Or(long, strings.TrimPrefix(name, "--"))
			case strings.HasPrefix(name, "-"):
				short = cmp.Or(short, strings.TrimPrefix(name, "-"))
			}
		}
		switch {
		case long == "" || root.Flags().Lookup(long) != nil:
		case flag.Value != "":
			root.Flags().StringArrayP(long, short, nil, flag.Help)
		default:
			root.Flags().BoolP(long, short, false, flag.Help)
		}
	}
	root.AddCommand(c.Commands(conn)...)
	root.SetArgs(args)
	return root.ExecuteContext(launch.WithGiven(ctx, c.given))
}
