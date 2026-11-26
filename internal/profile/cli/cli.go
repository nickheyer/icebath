package cli

import (
	"fmt"
	"text/tabwriter"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	profilev1 "github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1/profilev1connect"
)

type commands struct{}

func Register(host *cli.CLI) {
	host.Register(commands{})
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := profilev1connect.NewProfileServiceClient(conn)
	profiles := &cobra.Command{
		Use:   "profiles",
		Short: "List profiles, the one in use marked",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := service.List(cmd.Context(), &profilev1.ListRequest{})
			if err != nil {
				return err
			}
			table := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(table, "\tNAME")
			for _, entry := range response.GetProfiles() {
				mark := ""
				if entry.GetActive() {
					mark = "*"
				}
				fmt.Fprintf(table, "%s\t%s\n", mark, entry.GetName())
			}
			return table.Flush()
		},
	}
	profile := &cobra.Command{
		Use:   "profile",
		Short: "Make, use and delete profiles",
	}
	var from string
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Make a profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := service.Create(cmd.Context(), &profilev1.CreateRequest{Name: args[0], From: from})
			return err
		},
	}
	create.Flags().StringVar(&from, "from", "", "profile whose settings the new one starts with")
	profile.AddCommand(
		create,
		&cobra.Command{
			Use:   "use <name>",
			Short: "Use a profile from now on",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := service.Use(cmd.Context(), &profilev1.UseRequest{Name: args[0]})
				return err
			},
		},
		&cobra.Command{
			Use:   "delete <name>",
			Short: "Delete a profile and everything kept for it",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := service.Delete(cmd.Context(), &profilev1.DeleteRequest{Name: args[0]})
				return err
			},
		},
	)
	return []*cobra.Command{profiles, profile}
}
