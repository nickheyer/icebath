// The sandbox command: where the agent's commands run, how each place stands with what to do about what failed, choosing one and setting it
// up, the same as the page of Sandbox does
package cli

import (
	"cmp"
	"errors"
	"io"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	picked "github.com/nickheyer/icebath/internal/profile"
	terminalv1 "github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1/terminalv1connect"
)

type commands struct{}

func Register(host *cli.CLI) {
	host.Register(commands{})
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := terminalv1connect.NewBackendServiceClient(conn)
	var profile string
	root := &cobra.Command{
		Use:     "sandbox",
		GroupID: cli.GroupAbilities,
		Short:   "Show where the agent's commands run and how each place stands",
		Long: "Lists the places the agent's commands can run, such as this computer, Docker or SSH, with the one in use marked and, for one " +
			"that can't run, what was checked and what to do. use chooses a place, and setup gets one ready, as /sandbox does.",
		Example: "  icebath sandbox\n  icebath sandbox use docker\n  icebath sandbox setup docker",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return list(cmd, service, profile) },
	}
	root.PersistentFlags().StringVar(&profile, "profile", "", "icebath profile to act for, instead of the one in use")
	root.AddCommand(&cobra.Command{
		Use:     "use <place>",
		Short:   "Run the agent's commands in a place from the next one on",
		Example: "  icebath sandbox use local\n  icebath sandbox use ssh",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			name, err := resolve(cmd, service, profile, args[0])
			if err != nil {
				return err
			}
			used, err := service.Use(cmd.Context(), &terminalv1.UseRequest{Profile: named(cmd, profile), Backend: name})
			if err != nil {
				return err
			}
			if used.GetHeld() {
				c.Say("⚠ "+used.GetNote(), cli.Yellow)
				return nil
			}
			c.Say(c.Tick() + " " + used.GetNote())
			return nil
		},
	}, &cobra.Command{
		Use:     "setup <place>",
		Short:   "Get a place ready before its first command, such as by building its image",
		Example: "  icebath sandbox setup docker",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolve(cmd, service, profile, args[0])
			if err != nil {
				return err
			}
			return setup(cmd, service, profile, name)
		},
	})
	return []*cobra.Command{root}
}

// Name of the place a word names, by its name or its title
func resolve(cmd *cobra.Command, service terminalv1connect.BackendServiceClient, profile, word string) (string, error) {
	listed, err := service.Backends(cmd.Context(), &terminalv1.BackendsRequest{Profile: named(cmd, profile)})
	if err != nil {
		return "", err
	}
	var names []string
	for _, place := range listed.GetBackends() {
		if strings.EqualFold(place.GetName(), word) || strings.EqualFold(place.GetTitle(), word) {
			return place.GetName(), nil
		}
		names = append(names, place.GetName())
	}
	return "", errors.New("There is no place to run commands named " + word + ". The places are " + strings.Join(names, ", ") + ".")
}

// Lists the places with their states, then what was checked of each that is not ready and what to do about what failed
func list(cmd *cobra.Command, service terminalv1connect.BackendServiceClient, profile string) error {
	c := cli.NewConsole(cmd)
	listed, err := service.Backends(cmd.Context(), &terminalv1.BackendsRequest{Profile: named(cmd, profile)})
	if err != nil {
		return err
	}
	var rows [][]string
	for _, place := range listed.GetBackends() {
		mark := ""
		if place.GetSelected() {
			mark = c.Paint("●", cli.Green)
		}
		state := c.Paint(place.GetState(), cli.Green)
		switch {
		case place.GetMissing():
			state = c.Paint(place.GetState(), cli.Red)
		case !place.GetReady():
			state = c.Paint(place.GetState(), cli.Yellow)
		}
		rows = append(rows, []string{mark, place.GetTitle(), place.GetName(), state})
	}
	c.Table([]string{"", "Place", "Name", "State"}, rows)
	if variable := listed.GetVariable(); variable != "" {
		c.Blank()
		c.Say("⚠ "+variable+" names the place in use, which wins over what you choose while it is set.", cli.Yellow)
	}
	for _, place := range listed.GetBackends() {
		if place.GetReady() {
			continue
		}
		c.Blank()
		c.Say(place.GetTitle()+": "+place.GetWhy(), cli.Bold)
		for _, check := range place.GetChecks() {
			mark := c.Tick()
			if !check.GetOk() {
				mark = c.Paint("✗", cli.Red)
			}
			c.Say("  " + mark + " " + check.GetLabel() + ": " + check.GetState())
			if !check.GetOk() && check.GetFix() != "" {
				c.Say("    "+check.GetFix(), cli.Dim)
			}
		}
	}
	return nil
}

// Readies the place, printing each line as it comes and how it ended
func setup(cmd *cobra.Command, service terminalv1connect.BackendServiceClient, profile, name string) error {
	c := cli.NewConsole(cmd)
	stream, err := service.Prepare(cmd.Context(), &terminalv1.PrepareRequest{Profile: named(cmd, profile), Backend: name})
	if err != nil {
		return err
	}
	defer stream.Close()
	for {
		response, err := stream.Receive()
		switch {
		case errors.Is(err, io.EOF):
			c.Say(c.Tick() + " " + name + " is set up.")
			return nil
		case err != nil:
			return err
		case response.GetDone() && response.GetError() != "":
			return c.Fail("✗ " + name + " is not set up. " + response.GetError())
		case response.GetDone():
			c.Say(c.Tick() + " " + name + " is set up.")
			return nil
		}
		c.Say("  " + response.GetLine())
	}
}

// Profile a command acts for: the one its --profile names, else the one -p picked for icebath, else the one in use
func named(cmd *cobra.Command, flag string) string {
	return cmp.Or(flag, picked.Picked(cmd.Context()))
}
