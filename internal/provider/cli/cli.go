package cli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/provider"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1/providerv1connect"
)

type commands struct {
	signins *SignIns
}

func Register(host *cli.CLI, signins *SignIns) {
	host.Register(commands{signins: signins})
}

func (c commands) Commands(conn *connect.Client) []*cobra.Command {
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
	var refresh, details bool
	models := &cobra.Command{
		Use:   "models <provider>",
		Short: "List a provider's models",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			response, err := service.Models(cmd.Context(), &providerv1.ModelsRequest{Provider: args[0], Refresh: refresh})
			if err != nil {
				return err
			}
			if !details {
				for _, model := range response.GetModels() {
					if _, err := fmt.Fprintln(cmd.OutOrStdout(), model); err != nil {
						return err
					}
				}
				return nil
			}
			table := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(table, "MODEL\tCONTEXT\tMAX OUTPUT\tINPUT $/M\tOUTPUT $/M\tCACHE $/M\tABILITIES\tNOTE")
			for _, model := range response.GetDetails() {
				fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", model.GetId(), cmp.Or(models.Tokens(model.GetContext()), "-"), cmp.Or(models.Tokens(model.GetMaxOutput()), "-"),
					price(model.GetInputPrice()), price(model.GetOutputPrice()), price(model.GetCacheReadPrice()), cmp.Or(model.GetAbilities(), "-"),
					cmp.Or(model.GetNote(), "-"))
			}
			return table.Flush()
		},
	}
	models.Flags().BoolVar(&refresh, "refresh", false, "read the online catalog and the provider's list again rather than what is kept")
	models.Flags().BoolVar(&details, "details", false, "show each model's window, output, prices and abilities")
	var usageProvider string
	var document bool
	usage := &cobra.Command{
		Use:   "usage",
		Short: "Show account rate-limit windows (the /usage block) without starting a session",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := service.Usage(cmd.Context(), &providerv1.UsageRequest{Provider: usageProvider})
			if err == nil && response.GetUsage() == nil {
				err = fmt.Errorf("No account usage available for provider '%s': no credential is configured for it, the provider has no usage "+
					"endpoint, or the fetch failed.", cmp.Or(usageProvider, "of this profile"))
			}
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), err)
				return exit(1)
			}
			reading := response.GetUsage()
			if document {
				encoded, err := json.MarshalIndent(usageDocument(reading), "", "  ")
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), string(encoded))
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), strings.Join(provider.UsageOf(reading).Lines(false), "\n"))
			return err
		},
	}
	usage.Flags().StringVar(&usageProvider, "provider", "", "provider of the profile to ask, the first connected one that can say when empty")
	usage.Flags().BoolVar(&document, "json", false, "print the reading as JSON")
	return append([]*cobra.Command{providers, models, usage}, authCommands(conn, c.signins)...)
}

// Price for a million tokens as dollars, - when not known
func price(text string) string {
	if text == "" {
		return "-"
	}
	return models.PerMillion(text)
}

// The icebath usage --json document, whose keys stay and only grow
func usageDocument(reading *providerv1.AccountUsage) map[string]any {
	nullable := func(text string) any {
		if text == "" {
			return nil
		}
		return text
	}
	windows := []any{}
	for _, window := range reading.GetWindows() {
		var used, resets any
		if window.Used != nil {
			used = window.GetUsed()
		}
		if window.GetResets() != nil {
			resets = window.GetResets().AsTime().Format("2006-01-02T15:04:05.000000Z07:00")
		}
		windows = append(windows, map[string]any{"label": window.GetLabel(), "used_percent": used, "resets_at": resets, "detail": window.GetDetail()})
	}
	return map[string]any{"provider": reading.GetProvider(), "source": reading.GetSource(), "title": reading.GetTitle(), "plan": nullable(reading.GetPlan()),
		"fetched_at": reading.GetFetched().AsTime().Format("2006-01-02T15:04:05.000000Z07:00"), "windows": windows,
		"details": append([]string{}, reading.GetDetails()...), "unavailable_reason": nullable(reading.GetUnavailable())}
}

// Failure of a command that printed its own reason, ending with its status
type exit int

func (e exit) Error() string { return "" }

func (e exit) ExitCode() int { return int(e) }

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
