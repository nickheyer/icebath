package mattermost

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Mattermost wizard of icebath gateway setup writing the server, token, allowlist and home channel to the home .env
func InteractiveSetup(ctx context.Context) error {
	cli, cfg := gateway.WizardCLI, gateway.WizardConfig
	info := func(lines ...string) {
		for _, line := range lines {
			cli.PrintInfo(line)
		}
	}
	cli.PrintHeader("Mattermost")
	declined, err := gateway.PlatformSetup.DeclinesReconfigure(ctx, "Mattermost", "Reconfigure Mattermost?", "MATTERMOST_TOKEN")
	if err != nil || declined {
		return err
	}
	info("Works with any self-hosted Mattermost instance.",
		"   1. In Mattermost: Integrations → Bot Accounts → Add Bot Account", "   2. Copy the bot token")
	gateway.WizardPrintln("")
	if url := cli.Prompt("Mattermost server URL (e.g. https://mm.example.com)", "", false); url != "" {
		if err := cfg.SaveEnvValue(ctx, "MATTERMOST_URL", strings.TrimRight(url, "/")); err != nil {
			return err
		}
	}
	token := cli.Prompt("Bot token", "", true)
	if token == "" {
		return nil
	}
	if err := cfg.SaveEnvValue(ctx, "MATTERMOST_TOKEN", token); err != nil {
		return err
	}
	cli.PrintSuccess("Mattermost token saved")
	gateway.WizardPrintln("")
	info("🔒 Security: Restrict who can use your bot",
		"   Find your user ID (the profile dialog does not show it):",
		"   Admins: System Console → User Management → Users → open your user.",
		"   Anyone: while logged in, open <server>/api/v4/users/me in a browser tab.",
		"   With the bot token: GET <server>/api/v4/users/username/<your-username>.",
		"   Copy the \"id\" field. /users/me with the bot token returns the bot's ID.")
	gateway.WizardPrintln("")
	if allowed := cli.Prompt("Allowed user IDs (comma-separated, leave empty for open access)", "", false); allowed != "" {
		if err := cfg.SaveEnvValue(ctx, "MATTERMOST_ALLOWED_USERS", strings.ReplaceAll(allowed, " ", "")); err != nil {
			return err
		}
		cli.PrintSuccess("Mattermost allowlist configured")
	} else {
		cli.PrintInfo("⚠️  No allowlist set - anyone who can message the bot can use it!")
	}
	gateway.WizardPrintln("")
	info("📬 Home Channel: where Icebath delivers cron job results and notifications.",
		"   To get a channel ID: click channel name → View Info → copy the ID",
		"   You can also set this later by typing /set-home in a Mattermost channel.")
	if home := pystr.Strip(cli.Prompt("Home channel ID (leave empty to set later with /set-home)", "", false)); home != "" {
		if err := cfg.SaveEnvValue(ctx, "MATTERMOST_HOME_CHANNEL", home); err != nil {
			return err
		}
	} else if removed, err := cfg.RemoveEnvValue(ctx, "MATTERMOST_HOME_CHANNEL"); err != nil {
		return err
	} else if removed {
		cli.PrintInfo("Home channel cleared.")
	}
	cli.PrintInfo("   Open config in your editor:  icebath config edit")
	return nil
}
