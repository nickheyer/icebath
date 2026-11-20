package raft

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Raft wizard of icebath gateway setup persisting RAFT_PROFILE to the home .env
func InteractiveSetup(ctx context.Context) error {
	gateway.WizardCLI.PrintHeader("Raft")
	existing, err := gateway.WizardConfig.GetEnvValue(ctx, "RAFT_PROFILE")
	if err != nil {
		return err
	}
	declined, err := gateway.PlatformSetup.DeclinesReconfigure(ctx, "Raft", "Reconfigure Raft?", "RAFT_PROFILE")
	if err != nil {
		return err
	}
	if declined {
		gateway.WizardCLI.PrintInfo("Keeping RAFT_PROFILE=" + existing + ".")
		return nil
	}
	for _, line := range []string{"Connect Icebath to Raft as an external agent.", "Create the External Agent in Raft first, then run:",
		"  raft agent login --server <server-url> --agent <agent-id> --profile-slug <slug>"} {
		gateway.WizardCLI.PrintInfo(line)
	}
	gateway.WizardPrintln("")
	profile := gateway.WizardCLI.Prompt("Raft profile slug", existing, false)
	if profile == "" {
		gateway.WizardCLI.PrintWarning("Raft profile slug is required; skipping Raft setup")
		return nil
	}
	if err := gateway.WizardConfig.SaveEnvValue(ctx, "RAFT_PROFILE", pystr.Strip(profile)); err != nil {
		return err
	}
	gateway.WizardPrintln("")
	gateway.WizardCLI.PrintSuccess("Raft configuration saved")
	gateway.WizardCLI.PrintInfo("Restart the gateway for changes to take effect: icebath gateway restart")
	return nil
}
