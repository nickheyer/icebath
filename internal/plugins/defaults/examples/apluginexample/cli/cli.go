package cli

import (
	"fmt"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	apluginexamplev1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/apluginexample/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/apluginexample/v1/apluginexamplev1connect"
)

type commands struct{}

func Register(registry *cli.Registry) {
	registry.Register(commands{})
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := apluginexamplev1connect.NewExampleServiceClient(conn)
	return []*cobra.Command{{
		Use:   "apluginexample",
		Short: "Print the example plugin's greeting",
		Long: "Prints the greeting of the example plugin. The example plugin shows how a plugin adds a command, a screen and settings to " +
			"icebath.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := service.Greet(cmd.Context(), &apluginexamplev1.GreetRequest{})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), response.GetGreeting())
			return err
		},
	}}
}
