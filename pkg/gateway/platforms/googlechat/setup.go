package googlechat

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// GCP walkthrough the wizard prints before asking for the Pub/Sub settings
const setupWalkthrough = `Google Chat needs a GCP project, a Pub/Sub topic + subscription,
and a Service Account with Pub/Sub Subscriber on the subscription.
Walkthrough:
  1. Create or select a GCP project; enable Google Chat API + Cloud Pub/Sub API.
  2. Create a Service Account (no project-level IAM role needed).
  3. Create a Pub/Sub topic (e.g. icebath-chat-events) and a Pull subscription.
  4. On the TOPIC: add chat-api-push@system.gserviceaccount.com as Pub/Sub Publisher.
  5. On the SUBSCRIPTION: grant your Service Account Pub/Sub Subscriber.
  6. Download the Service Account JSON key.
  7. Google Chat API console → Configuration: connection = Cloud Pub/Sub,
     point at the topic, enable 1:1 + group, restrict visibility.
  8. Install the bot in a space (fires ADDED_TO_SPACE and resolves its user_id).

Full guide: website/docs/user-guide/messaging/google_chat.md
`

// Google Chat wizard of icebath gateway setup writing the GCP settings, allowlist and home space to the home .env
func InteractiveSetup(ctx context.Context) error {
	cli, cfg := gateway.WizardCLI, gateway.WizardConfig
	declined, err := gateway.PlatformSetup.DeclinesReconfigure(ctx, "Google Chat", "Reconfigure Google Chat?", "GOOGLE_CHAT_SUBSCRIPTION_NAME")
	if err != nil || declined {
		return err
	}
	for _, line := range pystr.Splitlines(setupWalkthrough, false) {
		cli.PrintInfo(line)
	}
	for _, q := range []struct {
		question, env, required string
		password                bool
	}{
		{"GCP project ID (e.g. my-project)", "GOOGLE_CHAT_PROJECT_ID", "Project ID", false},
		{"Pub/Sub subscription (projects/<proj>/subscriptions/<sub>)", "GOOGLE_CHAT_SUBSCRIPTION_NAME", "Subscription", false},
		{"Path to Service Account JSON (or inline JSON)", "GOOGLE_CHAT_SERVICE_ACCOUNT_JSON", "", true},
	} {
		existing, err := cfg.GetEnvValue(ctx, q.env)
		if err != nil {
			return err
		}
		value := cli.Prompt(q.question, existing, q.password)
		if value == "" {
			if q.required == "" {
				continue
			}
			cli.PrintWarning(q.required + " is required — skipping Google Chat setup")
			return nil
		}
		if err := cfg.SaveEnvValue(ctx, q.env, pystr.Strip(value)); err != nil {
			return err
		}
	}
	if cli.PromptYesNo(ctx, "Restrict access to specific users? (recommended)", true) {
		existing, err := cfg.GetEnvValue(ctx, "GOOGLE_CHAT_ALLOWED_USERS")
		if err != nil {
			return err
		}
		if allowed := cli.Prompt("Allowed user emails (comma-separated)", existing, false); allowed != "" {
			if err := cfg.SaveEnvValue(ctx, "GOOGLE_CHAT_ALLOWED_USERS", strings.ReplaceAll(allowed, " ", "")); err != nil {
				return err
			}
			cli.PrintSuccess("Allowlist configured")
		} else if err := cfg.SaveEnvValue(ctx, "GOOGLE_CHAT_ALLOWED_USERS", ""); err != nil {
			return err
		}
	} else {
		if err := cfg.SaveEnvValue(ctx, "GOOGLE_CHAT_ALLOW_ALL_USERS", "true"); err != nil {
			return err
		}
		cli.PrintWarning("⚠️  Open access — anyone who can DM the bot can command it.")
	}
	existingHome, err := cfg.GetEnvValue(ctx, "GOOGLE_CHAT_HOME_CHANNEL")
	if err != nil {
		return err
	}
	if home := cli.Prompt("Home space for cron/notification delivery (e.g. spaces/AAAA, or empty)", existingHome, false); home != "" {
		if err := cfg.SaveEnvValue(ctx, "GOOGLE_CHAT_HOME_CHANNEL", pystr.Strip(home)); err != nil {
			return err
		}
	}
	gateway.WizardPrintln("")
	cli.PrintSuccess("Google Chat configuration saved to ~/.icebath/.env")
	cli.PrintInfo("Restart the gateway: icebath gateway restart")
	return nil
}
