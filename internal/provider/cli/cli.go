package cli

import (
	"cmp"
	"fmt"
	"text/tabwriter"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1/providerv1connect"
)

type commands struct{}

func Register(host *cli.CLI) {
	host.Register(commands{})
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := providerv1connect.NewProviderServiceClient(conn)
	return []*cobra.Command{
		{
			Use:   "providers",
			Short: "List the inference providers and which have a credential",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				response, err := service.List(cmd.Context(), &providerv1.ListRequest{})
				if err != nil {
					return err
				}
				table := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(table, "NAME\tPROVIDER\tFORMAT\tCREDENTIAL\tENDPOINT")
				for _, entry := range response.GetProviders() {
					credential := cmp.Or(entry.GetCredentialSource(), "-")
					if entry.GetLocal() && entry.GetCredentialSource() == "" {
						credential = "not needed"
					}
					fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", entry.GetName(), entry.GetDisplayName(), entry.GetApiMode(), credential, cmp.Or(entry.GetBaseUrl(), "-"))
				}
				return table.Flush()
			},
		},
		{
			Use:   "models <provider>",
			Short: "List the models a provider serves",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				response, err := service.Models(cmd.Context(), &providerv1.ModelsRequest{Provider: args[0]})
				if err != nil {
					return err
				}
				for _, model := range response.GetModels() {
					if _, err := fmt.Fprintln(cmd.OutOrStdout(), model); err != nil {
						return err
					}
				}
				return nil
			},
		},
	}
}
