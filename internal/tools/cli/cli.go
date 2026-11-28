// The tools commands: how the groups of the agent's tools stand, every tool, and turning tools on or off through the function the page of
// Tools uses
package cli

import (
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	picked "github.com/nickheyer/icebath/internal/profile"
	toolsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/tools/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/tools/v1/toolsv1connect"
)

// Exit status a command ends with, its lines already printed
type exit int

func (e exit) Error() string { return "" }

func (e exit) ExitCode() int { return int(e) }

type commands struct{}

func Register(host *cli.CLI) {
	host.Register(commands{})
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := toolsv1connect.NewToolServiceClient(conn)
	root := &cobra.Command{
		Use:     "tools",
		GroupID: cli.GroupAbilities,
		Short:   "Turn the agent's tools on or off",
		Long: "The agent can use only the tools that are on. With no command after it, tools shows how each group of tools stands. list shows " +
			"every tool, and enable and disable turn tools on or off, by the name of a tool or of a group.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "✗ There is no tools command called “%s”. Run icebath tools --help to see them.\n", args[0])
				return exit(2)
			}
			response, err := service.List(cmd.Context(), &toolsv1.ListRequest{Profile: picked.Picked(cmd.Context())})
			if err != nil {
				return err
			}
			var rows [][]string
			for _, group := range response.GetGroups() {
				state := group.GetState()
				if group.GetElsewhere() {
					state = "With the server"
				}
				rows = append(rows, []string{group.GetTitle(), state, group.GetNote()})
			}
			cli.NewConsole(cmd).Table([]string{"Group", "State", "Can't run"}, rows)
			return nil
		},
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List every tool and whether it is on",
		Long:  "Lists every tool of the agent under its group: its name, whether it is on, and what it lacks when it is on and can't run.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := service.List(cmd.Context(), &toolsv1.ListRequest{Profile: picked.Picked(cmd.Context())})
			if err != nil {
				return err
			}
			groups := map[string]bool{}
			for _, group := range response.GetGroups() {
				groups[group.GetTitle()] = true
			}
			var rows [][]string
			for _, tool := range response.GetTools() {
				if !groups[tool.GetGroup()] {
					continue
				}
				state := "On"
				if tool.GetDisabled() {
					state = "Off"
				}
				rows = append(rows, []string{tool.GetGroup(), tool.GetTitle(), tool.GetName(), state, tool.GetLacks()})
			}
			cli.NewConsole(cmd).Table([]string{"Group", "Tool", "Name", "State", "What it lacks"}, rows)
			return nil
		},
	}
	turn := func(on bool) *cobra.Command {
		use, short, long := "disable <tool or group>...", "Turn tools off", "Turns off the tools named, by the name of a tool or of a group."
		if on {
			use, short, long = "enable <tool or group>...", "Turn tools on", "Turns on the tools named, by the name of a tool or of a group."
		}
		return &cobra.Command{Use: use, Short: short, Long: long, Example: "  icebath tools disable web_search Browser", Args: cobra.MinimumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				response, err := service.Turn(cmd.Context(), &toolsv1.TurnRequest{Profile: picked.Picked(cmd.Context()), Words: args, On: on})
				if failure, ok := errors.AsType[*connect.Error](err); ok && failure.Code() != connect.CodeUnavailable {
					fmt.Fprintln(cmd.ErrOrStderr(), "✗ "+failure.Message())
					return exit(1)
				}
				if err != nil {
					return err
				}
				c := cli.NewConsole(cmd)
				if len(response.GetTurned()) == 0 {
					c.Failure(response.GetSaid())
					return exit(1)
				}
				c.Success(response.GetSaid())
				return nil
			}}
	}
	root.AddCommand(list, turn(true), turn(false))
	return []*cobra.Command{root}
}
