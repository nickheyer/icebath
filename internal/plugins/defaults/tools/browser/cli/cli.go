// The browser command: closes the browser that holds the user's own profile, so the agent's browser tools can copy its logins
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
		Use: "browser",
		Short: "Helpers for real-profile browsing (browser.use_real_profile). close-profile terminates the browser process tree holding your default " +
			"profile so Icebath can copy it — DESTRUCTIVE (unsaved tabs in that browser are lost). The agent runs this only after you approve " +
			"closing the browser.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	root.PersistentFlags().StringVar(&profile, "profile", "", "Profile whose browser to close, the one in use when empty")
	var browser string
	closeProfile := &cobra.Command{
		Use:   "close-profile",
		Short: "Close the browser locking your real profile (asks nothing — run only with the user's explicit OK; loses unsaved tabs)",
		Args:  cobra.NoArgs,
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
	closeProfile.Flags().StringVar(&browser, "browser", "", "Browser whose profile to close ("+strings.Join(c.browsers, ", ")+"), the default one when empty")
	root.AddCommand(closeProfile)
	return []*cobra.Command{root}
}
