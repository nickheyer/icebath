// The browser command: closes the browser that holds the user's own profile, so the agent's browser tools can copy its sign-ins
package cli

import (
	"fmt"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	browserv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/browser/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/browser/v1/browserv1connect"
)

type commands struct {
	// Keys of the browsers --browser takes
	browsers []string
}

func Register(registry *cli.Registry, browsers []string) {
	registry.Register(commands{browsers: browsers})
}

func (c commands) Commands(conn *connect.Client) []*cobra.Command {
	service := browserv1connect.NewBrowserServiceClient(conn)
	var profile string
	root := &cobra.Command{
		Use:     "browser",
		GroupID: cli.GroupAbilities,
		Short:   "Let the agent browse with your own browser profile",
		Long: "When the agent browses with your own browser profile, icebath copies your sign-ins from that profile. Your browser locks the " +
			"profile while it runs, so it must be closed first. close-profile closes it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	root.PersistentFlags().StringVar(&profile, "profile", "", "icebath profile to act for, instead of the one in use")
	var browser string
	closeProfile := &cobra.Command{
		Use:   "close-profile",
		Short: "Close the browser that holds your own profile",
		Long: "Closes your browser and every window it has open, so icebath can copy your browser profile. Tabs you have not saved are lost, " +
			"and close-profile does not ask first. The agent runs it only after you agree. --browser takes one of these names: " +
			strings.Join(c.browsers, ", ") + ".",
		Example: "  icebath browser close-profile --browser brave",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reply, err := service.CloseProfile(cmd.Context(), &browserv1.CloseProfileRequest{Profile: profile, Browser: browser})
			if err != nil {
				return err
			}
			if !reply.GetClosed() {
				fmt.Fprintln(cmd.ErrOrStderr(), "✗ "+reply.GetMessage())
				return cli.Exit(1)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "✓ "+reply.GetMessage())
			return nil
		},
	}
	closeProfile.Flags().StringVar(&browser, "browser", "", "Browser to close, such as chrome or brave, instead of your default one")
	root.AddCommand(closeProfile)
	return []*cobra.Command{root}
}
