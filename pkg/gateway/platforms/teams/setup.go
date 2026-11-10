package teams

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Label, env var and masking of each Teams credential the wizard asks for
var setupCredentials = []struct {
	label, env string
	password   bool
}{{"Client ID", "TEAMS_CLIENT_ID", false}, {"Client secret", "TEAMS_CLIENT_SECRET", true}, {"Tenant ID", "TEAMS_TENANT_ID", false}}

// Intro lines of the wizard, an empty line printing a blank line
var setupIntro = []string{
	"You'll need the Teams CLI. If you haven't already:", "  npm install -g @microsoft/teams.cli@preview",
	"  teams login", "", "Then expose port 3978 publicly (devtunnel / ngrok / cloudflared),", "and create your bot:",
	`  teams app create --name "Icebath" --endpoint "https://<tunnel>/api/messages"`, "",
	"The CLI will print CLIENT_ID, CLIENT_SECRET, and TENANT_ID. Paste them below.", "",
}

// Microsoft Teams wizard of icebath gateway setup writing the bot credentials and allowlist to the home .env
func InteractiveSetup(ctx context.Context) error {
	declined, err := gateway.PlatformSetup.DeclinesReconfigure(ctx, "Teams", "Reconfigure Teams?", "TEAMS_CLIENT_ID")
	if err != nil || declined {
		return err
	}
	for _, line := range setupIntro {
		if line == "" {
			gateway.WizardPrintln("")
		} else {
			gateway.WizardCLI.PrintInfo(line)
		}
	}
	for _, c := range setupCredentials {
		existing, err := gateway.WizardConfig.GetEnvValue(ctx, c.env)
		if err != nil {
			return err
		}
		value := gateway.WizardCLI.Prompt(c.label, existing, c.password)
		if value == "" {
			gateway.WizardCLI.PrintWarning(c.label + " is required — skipping Teams setup")
			return nil
		}
		if err := gateway.WizardConfig.SaveEnvValue(ctx, c.env, pystr.Strip(value)); err != nil {
			return err
		}
	}
	gateway.WizardPrintln("")
	gateway.WizardCLI.PrintInfo("To find your AAD object ID for the allowlist: teams status --verbose")
	if gateway.WizardCLI.PromptYesNo(ctx, "Restrict access to specific users? (recommended)", true) {
		existing, err := gateway.WizardConfig.GetEnvValue(ctx, "TEAMS_ALLOWED_USERS")
		if err != nil {
			return err
		}
		if allowed := gateway.WizardCLI.Prompt("Allowed AAD object IDs (comma-separated)", existing, false); allowed != "" {
			if err := gateway.WizardConfig.SaveEnvValue(ctx, "TEAMS_ALLOWED_USERS", strings.ReplaceAll(allowed, " ", "")); err != nil {
				return err
			}
			gateway.WizardCLI.PrintSuccess("Allowlist configured")
		} else if err := gateway.WizardConfig.SaveEnvValue(ctx, "TEAMS_ALLOWED_USERS", ""); err != nil {
			return err
		}
	} else {
		if err := gateway.WizardConfig.SaveEnvValue(ctx, "TEAMS_ALLOW_ALL_USERS", "true"); err != nil {
			return err
		}
		gateway.WizardCLI.PrintWarning("⚠️  Open access — anyone who can message the bot can command it.")
	}
	gateway.WizardPrintln("")
	gateway.WizardCLI.PrintSuccess("Teams configuration saved to ~/.icebath/.env")
	gateway.WizardCLI.PrintInfo("Install the app in Teams:  teams app install --id <teamsAppId>")
	gateway.WizardCLI.PrintInfo("Restart the gateway:       icebath gateway restart")
	return nil
}
