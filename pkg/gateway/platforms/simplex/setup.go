package simplex

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Env var and prompt of each SimpleX setting the wizard asks for
var setupPrompts = [][2]string{
	{"SIMPLEX_WS_URL", "Daemon WebSocket URL (default ws://127.0.0.1:5225)"},
	{"SIMPLEX_ALLOWED_USERS", "Allowed contactIds (comma-separated; blank=skip)"},
	{"SIMPLEX_GROUP_ALLOWED", "Allowed group IDs (comma-separated, or '*' for any; blank=disable groups)"},
	{"SIMPLEX_AUTO_ACCEPT", "Auto-accept incoming contact requests? (true/false, default true)"},
	{"SIMPLEX_HOME_CHANNEL", "Home channel contact/group ID (or empty)"},
}

// SimpleX Chat wizard of icebath gateway setup writing the daemon settings to the home .env
func InteractiveSetup(ctx context.Context) error {
	gateway.WizardCLI.PrintHeader("SimpleX Chat")
	declined, err := gateway.PlatformSetup.DeclinesReconfigure(ctx, "SimpleX", "Reconfigure SimpleX?", "SIMPLEX_WS_URL")
	if err != nil || declined {
		return err
	}
	for _, line := range []string{"Requirements:", "  1. simplex-chat daemon running (e.g. `simplex-chat -p 5225`).",
		"  2. Python package `websockets` installed (`pip install websockets`)."} {
		gateway.WizardCLI.PrintInfo(line)
	}
	for _, p := range setupPrompts {
		existing, err := gateway.WizardConfig.GetEnvValue(ctx, p[0])
		if err != nil {
			return err
		}
		suffix := ""
		if existing != "" {
			suffix = " [keep current]"
		}
		if value := gateway.WizardCLI.Prompt(p[1]+suffix, "", false); value != "" {
			if err := gateway.WizardConfig.SaveEnvValue(ctx, p[0], value); err != nil {
				return err
			}
		}
	}
	gateway.WizardCLI.PrintInfo("Done. Make sure the simplex-chat daemon is running before starting the gateway.")
	return nil
}
