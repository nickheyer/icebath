// The browser command: closes the browser that holds the user's own profile, so the agent's browser tools can copy its sign-ins
package cli

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	picked "github.com/nickheyer/icebath/internal/profile"
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
		Short:   "Connect the agent to your own browser, or close it",
		Long: "connect lets the agent browse in a browser of yours with remote debugging on, kept for every session until you disconnect. When " +
			"the agent browses with your own browser profile, icebath copies your sign-ins from that profile. Your browser locks the profile " +
			"while it runs, so it must be closed first, which close-profile does.",
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
			reply, err := service.CloseProfile(cmd.Context(), &browserv1.CloseProfileRequest{Profile: named(cmd, profile), Browser: browser})
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
	connectCommand := &cobra.Command{
		Use:   "connect [address]",
		Short: "Connect the agent to your own browser",
		Long: "Connects the agent to a Chrome, Edge or Brave browser with remote debugging on, and keeps its address so every session browses " +
			"there. With no address, connect finds or starts the browser on this computer, as connecting in /browser does.",
		Example: "  icebath browser connect\n  icebath browser connect http://127.0.0.1:9222",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			address := ""
			if len(args) > 0 {
				address = args[0]
			}
			return connectTo(cmd, service, profile, address)
		},
	}
	disconnect := &cobra.Command{
		Use:   "disconnect",
		Short: "Disconnect the agent from your own browser",
		Long:  "Disconnects the agent from your browser and forgets its address, so the agent opens a browser of its own again.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reply, err := service.Disconnect(cmd.Context(), &browserv1.DisconnectRequest{Profile: named(cmd, profile)})
			if err != nil {
				return err
			}
			// With a variable of the environment still naming a browser, the agent stays on it, which is no success to mark
			if reply.GetHeld() {
				fmt.Fprintln(cmd.OutOrStdout(), reply.GetNote())
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), "✓ "+reply.GetNote())
			return nil
		},
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show whether the agent uses your own browser",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reply, err := service.Status(cmd.Context(), &browserv1.StatusRequest{Profile: named(cmd, profile)})
			if err != nil {
				return err
			}
			switch {
			case reply.GetConnected() && reply.GetVariable() != "":
				fmt.Fprintln(cmd.OutOrStdout(), "Connected to your browser at "+reply.GetUrl()+", which "+reply.GetVariable()+" names.")
			case reply.GetConnected():
				fmt.Fprintln(cmd.OutOrStdout(), "Connected to your browser at "+reply.GetUrl()+".")
			default:
				fmt.Fprintln(cmd.OutOrStdout(), "Not connected. The agent opens a browser of its own.")
			}
			return nil
		},
	}
	root.AddCommand(closeProfile, connectCommand, disconnect, status)
	return []*cobra.Command{root}
}

// Connects to the browser at the address, printing each step as the server tells it and how it ended
func connectTo(cmd *cobra.Command, service browserv1connect.BrowserServiceClient, profile, address string) error {
	stream, err := service.Connect(cmd.Context(), &browserv1.ConnectRequest{Profile: named(cmd, profile), Url: address})
	if err != nil {
		return err
	}
	defer stream.Close()
	out := cmd.OutOrStdout()
	for {
		event, err := stream.Receive()
		switch {
		case errors.Is(err, io.EOF):
			return errors.New("The connect stopped without saying whether it worked.")
		case err != nil:
			return err
		}
		switch kind := event.GetEvent().(type) {
		case *browserv1.ConnectEvent_Progress:
			if kind.Progress.GetLevel() == "error" {
				fmt.Fprintln(cmd.ErrOrStderr(), "  ⚠ "+kind.Progress.GetMessage())
				continue
			}
			fmt.Fprintln(out, "  "+kind.Progress.GetMessage())
		case *browserv1.ConnectEvent_Connected:
			if !kind.Connected.GetConnected() {
				fmt.Fprintln(cmd.ErrOrStderr(), "✗ "+kind.Connected.GetNote())
				return cli.Exit(1)
			}
			fmt.Fprintln(out, "✓ "+kind.Connected.GetNote())
			return nil
		}
	}
}

// Profile a command acts for: the one its --profile names, else the one -p picked for icebath, else the one in use
func named(cmd *cobra.Command, flag string) string {
	return cmp.Or(flag, picked.Picked(cmd.Context()))
}
