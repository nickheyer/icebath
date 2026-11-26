package cli

import (
	"cmp"
	"fmt"
	"strings"
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
	providers := &cobra.Command{
		Use:   "providers",
		Short: "List the providers of the profile and whether they're connected",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := service.List(cmd.Context(), &providerv1.ListRequest{})
			if err != nil {
				return err
			}
			table := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(table, "NAME\tKIND\tTITLE\tACCOUNT\tKEY\tBASE URL")
			for _, entry := range response.GetProviders() {
				fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n", entry.GetName(), entry.GetSpec(), entry.GetDisplayName(), cmp.Or(entry.GetAccount(), "-"),
					credential(entry.GetCredentialOrigin(), entry.GetCredentialSource(), entry.GetLocal()), cmp.Or(entry.GetBaseUrl(), "-"))
			}
			return table.Flush()
		},
	}
	var name, title string
	add := &cobra.Command{
		Use:   "add <kind>",
		Short: "Add a provider of the kind to the profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			response, err := service.Add(cmd.Context(), &providerv1.AddRequest{Spec: args[0], Name: name, Title: title})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), response.GetProvider().GetName())
			return err
		},
	}
	add.Flags().StringVar(&name, "name", "", "name in the profile, the kind's name when empty")
	add.Flags().StringVar(&title, "title", "", "name the provider is shown by")
	providers.AddCommand(
		&cobra.Command{
			Use:   "kinds",
			Short: "List the kinds of provider a profile can add",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				response, err := service.Specs(cmd.Context(), &providerv1.SpecsRequest{})
				if err != nil {
					return err
				}
				table := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(table, "KIND\tTITLE\tAPI\tFOUND\tIN PROFILE")
				for _, spec := range response.GetSpecs() {
					fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", spec.GetName(), spec.GetDisplayName(), spec.GetApiMode(),
						credential(spec.GetDetectedOrigin(), spec.GetDetectedSource(), spec.GetLocal()), cmp.Or(strings.Join(spec.GetProviders(), ", "), "-"))
				}
				return table.Flush()
			},
		},
		add,
		&cobra.Command{
			Use:   "remove <name>",
			Short: "Remove a provider from the profile, with its accounts",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := service.Remove(cmd.Context(), &providerv1.RemoveRequest{Provider: args[0]})
				return err
			},
		},
	)
	models := &cobra.Command{
		Use:   "models <provider>",
		Short: "List a provider's models",
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
	}
	return []*cobra.Command{providers, models}
}

// Where a credential comes from: saved, the variable or program it is read from, or not needed
func credential(origin providerv1.Origin, source string, local bool) string {
	switch origin {
	case providerv1.Origin_ORIGIN_SAVED:
		return "saved"
	case providerv1.Origin_ORIGIN_ENVIRONMENT, providerv1.Origin_ORIGIN_EXTERNAL:
		return source
	case providerv1.Origin_ORIGIN_UNNEEDED:
		return "not needed"
	}
	if local {
		return "not needed"
	}
	return "-"
}
