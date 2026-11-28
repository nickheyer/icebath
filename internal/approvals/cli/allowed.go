package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	picked "github.com/nickheyer/icebath/internal/profile"
	approvalsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/approvals/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/approvals/v1/approvalsv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Headings of the kinds of entry, in the order the allowlist keeps them
var headings = map[string]string{"kind": "Kinds of risky action", "pattern": "Patterns", "command": "Exact commands"}

func list(service approvalsv1connect.ApprovalsServiceClient) *cobra.Command {
	return &cobra.Command{Use: "list", Short: "Show what runs without asking", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		out := cmd.OutOrStdout()
		response, err := service.List(cmd.Context(), &approvalsv1.ListRequest{Profile: picked.Picked(cmd.Context())})
		if err != nil {
			return err
		}
		if winning := response.GetWinning(); winning != "" {
			fmt.Fprintf(out, "⚠ %s\n\n", winning)
		}
		if len(response.GetEntries()) == 0 {
			fmt.Fprintln(out, "Nothing runs without asking yet. Let a command run so with icebath approvals allow and the command.")
			return nil
		}
		kind := ""
		for _, entry := range response.GetEntries() {
			if entry.GetKind() != kind {
				if kind != "" {
					fmt.Fprintln(out)
				}
				kind = entry.GetKind()
				fmt.Fprintln(out, headings[kind])
			}
			fmt.Fprintf(out, "  %s  %s\n", entry.GetTitle(), entry.GetThrough())
		}
		return nil
	}}
}

func allow(service approvalsv1connect.ApprovalsServiceClient) *cobra.Command {
	return &cobra.Command{Use: "allow <command or pattern>", Short: "Let a command or a pattern run without asking",
		Long:    "Lets a command run without asking. A pattern such as \"npm run *\" lets every command that matches run so, * standing for anything.",
		Example: "  icebath approvals allow \"git status\"\n  icebath approvals allow \"npm run *\"",
		Args:    cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			entry := strings.Join(args, " ")
			response, err := service.Allow(cmd.Context(), &approvalsv1.AllowRequest{Profile: picked.Picked(cmd.Context()), Patterns: []string{entry}})
			if err != nil {
				return err
			}
			if len(response.GetAdded()) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "%s runs without asking already.\n", entry)
				return nil
			}
			runs := "run"
			if response.GetSize() == 1 {
				runs = "runs"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ %s now runs without asking. %s %s without asking now.\n", entry,
				strs.Count(int(response.GetSize()), "command or pattern"), runs)
			return nil
		}}
}

func remove(service approvalsv1connect.ApprovalsServiceClient) *cobra.Command {
	return &cobra.Command{Use: "remove <command or pattern>", Short: "Ask again before a command or a pattern",
		Example: "  icebath approvals remove \"npm run *\"", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			entry := strings.Join(args, " ")
			listed, err := service.List(cmd.Context(), &approvalsv1.ListRequest{Profile: picked.Picked(cmd.Context())})
			if err != nil {
				return err
			}
			held := false
			for _, value := range listed.GetAllowed() {
				held = held || value == entry
			}
			if !held {
				fmt.Fprintf(cmd.OutOrStdout(), "✗ %s does not run without asking, so there is nothing to remove. icebath approvals list shows what does.\n", entry)
				return exit(1)
			}
			if _, err := service.Disallow(cmd.Context(), &approvalsv1.DisallowRequest{Profile: picked.Picked(cmd.Context()), Patterns: []string{entry}}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ The agent asks again before %s.\n", entry)
			return nil
		}}
}

func mode(service approvalsv1connect.ApprovalsServiceClient) *cobra.Command {
	return &cobra.Command{Use: "mode <manual|smart|off>", Short: "Set when the agent asks",
		Long: "Sets when the agent asks before a risky command, for every session of the profile: manual asks before every one, smart lets a " +
			"helper model pass clearly safe ones, and off never asks.",
		Args: cobra.ExactArgs(1), ValidArgs: []string{"manual", "smart", "off"}, RunE: func(cmd *cobra.Command, args []string) error {
			response, err := service.Mode(cmd.Context(), &approvalsv1.ModeRequest{Profile: picked.Picked(cmd.Context()), Mode: args[0]})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ %s\n", response.GetNote())
			return nil
		}}
}
