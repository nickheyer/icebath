// The camofox command: points the profile at a camofox-browser server and checks it answers
package cli

import (
	"cmp"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	camofoxconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/camofox/config"
	picked "github.com/nickheyer/icebath/internal/profile"
	camofoxv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/camofox/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/camofox/v1/camofoxv1connect"
)

// Lines saying where to get a Camofox server and how to start one
var serverHelp = []string{
	"  How to run a Camofox server: https://github.com/jo-inc/camofox-browser",
	"  On this computer: npm install && npm start",
	"  With Docker: docker run -p 9377:9377 -e CAMOFOX_PORT=9377 jo-inc/camofox-browser",
}

type commands struct{}

func Register(registry *cli.Registry) {
	registry.Register(commands{})
}

// Prints how the server stands, an exit status of 1 when none is set or it does not answer
func show(cmd *cobra.Command, st *camofoxv1.StatusResponse) error {
	c := cli.NewConsole(cmd)
	if st.GetUrl() == "" {
		c.Failure("No Camofox server is set. Give its address with icebath camofox setup.")
		for _, line := range serverHelp {
			c.Say(line)
		}
		return cli.Exit(1)
	}
	rows := [][2]string{{"Server address", st.GetUrl()}, {"Camofox user", st.GetIdentity()}}
	if vnc := st.GetVnc(); vnc != "" && st.GetReachable() {
		rows = append(rows, [2]string{"Watch over VNC", vnc})
	}
	c.Pairs("Camofox", rows)
	if !st.GetReachable() {
		c.Failure("The server does not answer. Start it, and check its address, Server address on the Camofox page of /browser.")
		for _, line := range serverHelp {
			c.Say(line)
		}
		return cli.Exit(1)
	}
	c.Success("The server answers.")
	return nil
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := camofoxv1connect.NewCamofoxServiceClient(conn)
	var profile string
	root := &cobra.Command{
		Use:     "camofox",
		GroupID: cli.GroupAbilities,
		Short:   "Set up and check the Camofox browser server",
		Long: "Camofox is a browser server you run yourself, which the agent can browse with. Use setup to give icebath its address, and " +
			"status to check that it answers. With no subcommand, camofox shows the status.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := service.Status(cmd.Context(), &camofoxv1.StatusRequest{Profile: named(cmd, profile)})
			if err != nil {
				return err
			}
			return show(cmd, st)
		},
	}
	root.PersistentFlags().StringVar(&profile, "profile", "", "icebath profile to act for, instead of the one in use")
	var address string
	setup := &cobra.Command{
		Use:   "setup",
		Short: "Save the address of your Camofox server",
		Long: "Saves the address of your Camofox server, and an API key when it needs one, in this profile. icebath does not install or " +
			"start Camofox. The agent can use the Camofox browser tools from its next reply.",
		Example: "  icebath camofox setup --url http://localhost:9377",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			console := cli.NewConsole(cmd)
			console.Say("Camofox is a server you run yourself, as icebath does not install or start it.")
			chosen := address
			if !cmd.Flags().Changed("url") {
				current, err := service.Status(cmd.Context(), &camofoxv1.StatusRequest{Profile: named(cmd, profile)})
				if err != nil {
					return err
				}
				chosen = console.Prompt("Address of your Camofox server", cmp.Or(current.GetUrl(), camofoxconfig.DefaultURL), false)
			}
			key := console.Prompt("API key of your Camofox server, or press enter if it needs none", "", true)
			reply, err := service.Setup(cmd.Context(), &camofoxv1.SetupRequest{Profile: named(cmd, profile), Url: chosen, ApiKey: key})
			if err != nil {
				return err
			}
			console.Success("Saved the address as Server address on the Camofox page of /browser. The agent can use the Camofox tools from " +
				"its next reply.")
			return show(cmd, reply.GetStatus())
		},
	}
	setup.Flags().StringVar(&address, "url", "", "Address of the Camofox server, instead of asking for it")
	status := &cobra.Command{
		Use:   "status",
		Short: "Show your Camofox server and whether it answers",
		Long:  "Shows the address of your Camofox server and whether it answers.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := service.Status(cmd.Context(), &camofoxv1.StatusRequest{Profile: named(cmd, profile)})
			if err != nil {
				return err
			}
			return show(cmd, st)
		},
	}
	root.AddCommand(setup, status)
	return []*cobra.Command{root}
}

// Profile a command acts for: the one its --profile names, else the one -p picked for icebath, else the one in use
func named(cmd *cobra.Command, flag string) string {
	return cmp.Or(flag, picked.Picked(cmd.Context()))
}
