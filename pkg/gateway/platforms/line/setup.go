package line

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Env var, prompt and masking of each LINE credential the wizard asks for
var setupPrompts = []struct {
	name, question string
	secret         bool
}{
	{"LINE_CHANNEL_ACCESS_TOKEN", "Channel access token", true},
	{"LINE_CHANNEL_SECRET", "Channel secret", true},
	{"LINE_PUBLIC_URL", "Public HTTPS base URL (optional, e.g. https://my-tunnel.example.com)", false},
	{"LINE_ALLOWED_USERS", "Allowed user IDs (comma-separated; blank=skip)", false},
}

// LINE Messaging API wizard of icebath gateway setup writing the channel credentials to the home .env
func InteractiveSetup(ctx context.Context) error {
	gateway.WizardCLI.PrintHeader("LINE Messaging API")
	declined, err := gateway.PlatformSetup.DeclinesReconfigure(ctx, "LINE", "Reconfigure LINE?", "LINE_CHANNEL_ACCESS_TOKEN")
	if err != nil || declined {
		return err
	}
	gateway.WizardCLI.PrintInfo("Create a Messaging API channel at https://developers.line.biz/console/ then copy the values below.")
	for _, p := range setupPrompts {
		existing, err := gateway.WizardConfig.GetEnvValue(ctx, p.name)
		if err != nil {
			return err
		}
		suffix := ""
		if existing != "" {
			suffix = " [keep current]"
		}
		if value := gateway.WizardCLI.Prompt(p.question+suffix, "", p.secret); value != "" {
			if err := gateway.WizardConfig.SaveEnvValue(ctx, p.name, value); err != nil {
				return err
			}
		}
	}
	gateway.WizardCLI.PrintInfo("Done. Set the webhook URL in the LINE console to <your-public-url>/line/webhook and enable 'Use webhook'.")
	return nil
}
