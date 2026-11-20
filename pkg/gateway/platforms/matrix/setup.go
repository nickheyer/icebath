package matrix

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Matrix wizard of icebath gateway setup writing the homeserver, auth, E2EE, allowlist and home room to the home .env
func InteractiveSetup(ctx context.Context) error {
	cli, cfg := gateway.WizardCLI, gateway.WizardConfig
	cli.PrintHeader("Matrix")
	declined, err := gateway.PlatformSetup.DeclinesReconfigure(ctx, "Matrix", "Reconfigure Matrix?", "MATRIX_ACCESS_TOKEN", "MATRIX_PASSWORD")
	if err != nil || declined {
		return err
	}
	for _, line := range []string{"Works with any Matrix homeserver (Synapse, Conduit, Dendrite, or matrix.org).",
		"   1. Create a bot user on your homeserver, or use your own account",
		"   2. Get an access token from Element, or provide user ID + password"} {
		cli.PrintInfo(line)
	}
	ask := func(key, question string, password bool) (string, error) {
		value := cli.Prompt(question, "", password)
		if value == "" {
			return value, nil
		}
		saved := value
		if key == "MATRIX_HOMESERVER" {
			saved = strings.TrimRight(value, "/")
		}
		return value, cfg.SaveEnvValue(ctx, key, saved)
	}
	if _, err := ask("MATRIX_HOMESERVER", "Homeserver URL (e.g. https://matrix.example.org)", false); err != nil {
		return err
	}
	cli.PrintInfo("Auth: provide an access token (recommended), or user ID + password.")
	token, err := ask("MATRIX_ACCESS_TOKEN", "Access token (leave empty for password login)", true)
	if err != nil {
		return err
	}
	if token != "" {
		if _, err := ask("MATRIX_USER_ID", "User ID (@bot:server — optional, will be auto-detected)", false); err != nil {
			return err
		}
		cli.PrintSuccess("Matrix access token saved")
	} else {
		if _, err := ask("MATRIX_USER_ID", "User ID (@bot:server)", false); err != nil {
			return err
		}
		password, err := ask("MATRIX_PASSWORD", "Password", true)
		if err != nil {
			return err
		}
		if password != "" {
			cli.PrintSuccess("Matrix credentials saved")
		}
	}
	if token == "" {
		saved, err := cfg.GetEnvValue(ctx, "MATRIX_PASSWORD")
		if err != nil || saved == "" {
			return err
		}
	}
	if cli.PromptYesNo(ctx, "Enable end-to-end encryption (E2EE)?", false) {
		if err := cfg.SaveEnvValue(ctx, "MATRIX_ENCRYPTION", "true"); err != nil {
			return err
		}
		cli.PrintSuccess("E2EE enabled")
		if !checkE2EEDeps() {
			cli.PrintWarning("This build has no olm implementation for E2EE. " + e2eeInstallHint)
		}
	}
	cli.PrintInfo("🔒 Security: Restrict who can use your bot")
	cli.PrintInfo("   Matrix user IDs look like @username:server")
	if allowed := cli.Prompt("Allowed user IDs (comma-separated, leave empty for open access)", "", false); allowed != "" {
		if err := cfg.SaveEnvValue(ctx, "MATRIX_ALLOWED_USERS", strings.ReplaceAll(allowed, " ", "")); err != nil {
			return err
		}
		cli.PrintSuccess("Matrix allowlist configured")
	} else {
		cli.PrintInfo("⚠️  No allowlist set - anyone who can message the bot can use it!")
	}
	for _, line := range []string{"📬 Home Room: where Icebath delivers cron job results and notifications.",
		"   Room IDs look like !abc123:server (shown in Element room settings)",
		"   You can also set this later by typing /set-home in a Matrix room.",
		"Leave blank to clear a previously saved home room (cron / notifications)."} {
		cli.PrintInfo(line)
	}
	if room := pystr.Strip(cli.Prompt("Home room ID (leave empty to set later with /set-home)", "", false)); room != "" {
		return cfg.SaveEnvValue(ctx, "MATRIX_HOME_ROOM", room)
	}
	removed, err := cfg.RemoveEnvValue(ctx, "MATRIX_HOME_ROOM")
	if err == nil && removed {
		cli.PrintInfo("Home room cleared.")
	}
	return err
}
