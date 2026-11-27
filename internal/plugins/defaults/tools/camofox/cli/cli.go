// The camofox command: points the profile at a camofox-browser server and checks it answers
package cli

import (
	"cmp"
	"fmt"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	camofoxconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/camofox/config"
	camofoxv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/camofox/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/camofox/v1/camofoxv1connect"
)

const serverHelp = "Server setup: https://github.com/jo-inc/camofox-browser  (native: npm install && npm start; Docker: docker run -p 9377:9377 " +
	"-e CAMOFOX_PORT=9377 jo-inc/camofox-browser)"

type commands struct{}

func Register(registry *cli.Registry) {
	registry.Register(commands{})
}

// Prints how the server stands, an exit status of 1 when none is set or it does not answer
func show(cmd *cobra.Command, st *camofoxv1.StatusResponse) error {
	out := cmd.OutOrStdout()
	if st.GetUrl() == "" {
		fmt.Fprintln(out, "CAMOFOX_URL is not set. Run: icebath camofox setup")
		fmt.Fprintln(out, serverHelp)
		return cli.Exit(1)
	}
	fmt.Fprintf(out, "Camofox server: %s\n", st.GetUrl())
	fmt.Fprintf(out, "Identity:       %s\n", st.GetIdentity())
	if !st.GetReachable() {
		fmt.Fprintln(out, "Status:         not reachable. Start your server and check CAMOFOX_URL.")
		fmt.Fprintln(out, serverHelp)
		return cli.Exit(1)
	}
	fmt.Fprintln(out, "Status:         reachable")
	if vnc := st.GetVnc(); vnc != "" {
		fmt.Fprintf(out, "VNC:            %s\n", vnc)
	}
	return nil
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := camofoxv1connect.NewCamofoxServiceClient(conn)
	var profile string
	root := &cobra.Command{
		Use:   "camofox",
		Short: "Set up and check the Camofox browser server",
		Long:  "Camofox browser backend: point Icebath at a camofox-browser server and check it.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := service.Status(cmd.Context(), &camofoxv1.StatusRequest{Profile: profile})
			if err != nil {
				return err
			}
			return show(cmd, st)
		},
	}
	root.PersistentFlags().StringVar(&profile, "profile", "", "Profile whose Camofox server to use, the one in use when empty")
	var address string
	setup := &cobra.Command{
		Use:   "setup",
		Short: "Save the Camofox server address (and an optional API key) to this profile",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			console := cli.NewConsole(cmd)
			fmt.Fprintln(cmd.OutOrStdout(), "Camofox is an externally managed server; Icebath does not install or start it.")
			chosen := address
			if !cmd.Flags().Changed("url") {
				current, err := service.Status(cmd.Context(), &camofoxv1.StatusRequest{Profile: profile})
				if err != nil {
					return err
				}
				chosen = console.Prompt("Camofox server URL", cmp.Or(current.GetUrl(), camofoxconfig.DefaultURL), false)
			}
			key := console.Prompt("Camofox API key (optional, Enter to skip)", "", true)
			reply, err := service.Setup(cmd.Context(), &camofoxv1.SetupRequest{Profile: profile, Url: chosen, ApiKey: key})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Saved to this profile (browser.camofox.url). The camofox browser tools are offered from the next turn.")
			return show(cmd, reply.GetStatus())
		},
	}
	setup.Flags().StringVar(&address, "url", "", "Camofox server URL (default: prompt, "+camofoxconfig.DefaultURL+")")
	status := &cobra.Command{
		Use:   "status",
		Short: "Show the configured Camofox server and whether it answers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := service.Status(cmd.Context(), &camofoxv1.StatusRequest{Profile: profile})
			if err != nil {
				return err
			}
			return show(cmd, st)
		},
	}
	root.AddCommand(setup, status)
	return []*cobra.Command{root}
}
