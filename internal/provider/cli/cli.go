package cli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/provider"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1/providerv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
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
		Use:     "providers",
		GroupID: cli.GroupModels,
		Short:   "List your providers and whether each is connected",
		Long: "A provider is a company or server that runs models, such as Anthropic or OpenRouter. providers lists the providers of this " +
			"profile with their kind, account and API key, and where each key comes from.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := service.List(cmd.Context(), &providerv1.ListRequest{})
			if err != nil {
				return err
			}
			console := cli.NewConsole(cmd)
			if len(response.GetProviders()) == 0 {
				console.Say("No providers yet. Add one with `icebath providers add <kind>` or `icebath auth add <kind>`, or open /providers in icebath.")
				return nil
			}
			var rows [][]string
			for _, entry := range response.GetProviders() {
				rows = append(rows, []string{entry.GetName(), entry.GetSpec(), entry.GetDisplayName(), cmp.Or(entry.GetAccount(), "None"),
					credential(entry.GetCredentialOrigin(), entry.GetCredentialSource(), entry.GetLocal()), cmp.Or(entry.GetBaseUrl(), "Not set")})
			}
			console.Table([]string{"Name", "Kind", "Title", "Account", "API key", "Address"}, rows)
			return nil
		},
	}
	var name, title string
	add := &cobra.Command{
		Use:   "add <kind>",
		Short: "Add a provider of this kind to the profile",
		Long: "Adds a provider of the kind, as providers kinds lists it, to this profile and prints its name. Give it another name with " +
			"--name, for example to add the same kind twice.",
		Example: "  icebath providers add anthropic\n  icebath providers add openai --name work-openai --title \"Work OpenAI\"",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			response, err := service.Add(cmd.Context(), &providerv1.AddRequest{Spec: args[0], Name: name, Title: title})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), response.GetProvider().GetName())
			return err
		},
	}
	add.Flags().StringVar(&name, "name", "", "Name of the provider in the profile, instead of the kind's name")
	add.Flags().StringVar(&title, "title", "", "Name the provider is shown by")
	providers.AddCommand(
		&cobra.Command{
			Use:   "kinds",
			Short: "List the kinds of provider you can add",
			Long: "Lists every kind of provider icebath can add, whether it found an API key for it and which of your providers are of " +
				"that kind.",
			Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				response, err := service.Specs(cmd.Context(), &providerv1.SpecsRequest{})
				if err != nil {
					return err
				}
				var rows [][]string
				for _, spec := range response.GetSpecs() {
					rows = append(rows, []string{spec.GetName(), spec.GetDisplayName(), spec.GetDescription(), formats[spec.GetApiMode()],
						credential(spec.GetDetectedOrigin(), spec.GetDetectedSource(), spec.GetLocal()), cmp.Or(strings.Join(spec.GetProviders(), ", "), "None")})
				}
				cli.NewConsole(cmd).Table([]string{"Kind", "Title", "What it is", "Request format", "API key", "Your providers"}, rows)
				return nil
			},
		},
		add,
		&cobra.Command{
			Use:   "remove <name>",
			Short: "Remove a provider and its accounts from the profile",
			Long:  "Removes the provider from this profile, with every account kept for it.",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := service.Remove(cmd.Context(), &providerv1.RemoveRequest{Provider: args[0]})
				return err
			},
		},
	)
	var refresh, details bool
	models := &cobra.Command{
		Use:     "models <provider>",
		GroupID: cli.GroupModels,
		Short:   "List the models of a provider",
		Long: "Lists the models that the provider offers. With --details, models also shows each model's context window, output limit, " +
			"prices and abilities.",
		Example: "  icebath models anthropic --details",
		Args:    cobra.ExactArgs(1),
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
			var rows [][]string
			for _, model := range response.GetDetails() {
				rows = append(rows, []string{model.GetId(), cmp.Or(models.Tokens(model.GetContext()), "Not known"), cmp.Or(models.Tokens(model.GetMaxOutput()), "Not known"),
					price(model.GetInputPrice()), price(model.GetOutputPrice()), price(model.GetCacheReadPrice()), cmp.Or(model.GetAbilities(), "Not known"),
					model.GetNote()})
			}
			cli.NewConsole(cmd).Table([]string{"Model", "Context window", "Max output", "Input per million", "Output per million", "Cached per million",
				"Can do", "Note"}, rows)
			return nil
		},
	}
	models.Flags().BoolVar(&refresh, "refresh", false, "Read the lists of models again instead of the copies kept")
	models.Flags().BoolVar(&details, "details", false, "Show each model's context window, output limit, prices and abilities")
	var usageProvider string
	var document bool
	usage := &cobra.Command{
		Use:     "usage",
		GroupID: cli.GroupModels,
		Short:   "Show the usage limits of your account",
		Long:    "Shows how much of your account's usage limits you have used and when they reset, as /usage does, without starting a session.",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := service.Usage(cmd.Context(), &providerv1.UsageRequest{Provider: usageProvider})
			if err == nil && response.GetUsage() == nil {
				err = fmt.Errorf("icebath can't say how much of the limits of %s you have used. It may have no account, no way to report usage, or "+
					"it did not answer.", cmp.Or(usageProvider, "your providers"))
			}
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "✗ "+strings.TrimPrefix(tui.Reason(err), "✗ "))
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
	usage.Flags().StringVar(&usageProvider, "provider", "", "Provider to ask, instead of the first connected one that can tell")
	usage.Flags().BoolVar(&document, "json", false, "Print the result as JSON")
	return append([]*cobra.Command{providers, models, usage}, authCommands(conn, c.signins)...)
}

// Price for a million tokens as dollars, said in words when free or not known
func price(text string) string {
	switch text = models.PerMillion(text); {
	case strings.TrimSpace(text) == "" || text == "unknown":
		return "Not known"
	case text == "free":
		return "Free"
	}
	return text
}

// Request formats in words, by the name a kind gives
var formats = map[string]string{string(provider.ModeChatCompletions): "Chat completions", string(provider.ModeAnthropicMessages): "Anthropic messages",
	string(provider.ModeCodexResponses): "OpenAI responses", string(provider.ModeGeminiNative): "Gemini", string(provider.ModeBedrockConverse): "Bedrock Converse"}

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
		return "Kept by icebath"
	case providerv1.Origin_ORIGIN_ENVIRONMENT, providerv1.Origin_ORIGIN_EXTERNAL:
		return source
	case providerv1.Origin_ORIGIN_UNNEEDED:
		return "Not needed"
	}
	if local {
		return "Not needed"
	}
	return "Not set"
}
