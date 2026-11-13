package bluebubbles

import (
	"context"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const installHint = "BlueBubbles: BLUEBUBBLES_SERVER_URL/BLUEBUBBLES_PASSWORD not configured"

// System prompt platform hint, cached prompt bytes kept stable
const platformHint = "You are texting via iMessage (BlueBubbles). Replies arrive as plain text bubbles, so write like a person " +
	"texting: short and conversational, answer first, no preamble or recap. Markdown does not render and is " +
	"stripped, so skip headers, tables, code fences and backticks; for a few items use short lines or a " +
	"sentence rather than nested bullets. Put a command or code snippet on its own line as plain text so it " +
	"can be copied. Write links as bare URLs (iMessage auto-links them). " +
	"You can send files natively: write MEDIA:/absolute/path/to/file in your response. " +
	"Images (.jpg, .png, .heic) appear as photos and other files arrive as attachments."

// Dependency check, always met since net/http is compiled into the binary
func checkRequirements() bool { return true }

// Whether server URL and password are present in the config extra or the env
func isConnected(ctx context.Context, cfg *gateway.PlatformConfig) (bool, error) {
	if cfg != nil && pyrepr.Truthy(cfg.Extra["server_url"]) && pyrepr.Truthy(cfg.Extra["password"]) {
		return true, nil
	}
	return gateway.EnvIsConnected("BLUEBUBBLES_SERVER_URL", "BLUEBUBBLES_PASSWORD")(ctx, cfg)
}

// One shot connect, text send and disconnect against the BlueBubbles REST API for cron without a live gateway
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	cfg := gateway.NewPlatformConfig()
	cfg.Extra = map[string]any{}
	if pconfig != nil {
		for k, v := range pconfig.Extra {
			cfg.Extra[k] = v
		}
	}
	fail := func(err error) (map[string]any, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		slog.Debug("BlueBubbles standalone send failed", "error", err)
		return gateway.SendError("BlueBubbles send failed: " + pyrepr.ExceptionMessage(err)), nil
	}
	adapter, err := NewBlueBubblesAdapter(ctx, cfg)
	if err != nil {
		return fail(err)
	}
	connected, err := adapter.Connect(ctx, false)
	if err != nil {
		return fail(err)
	}
	if !connected {
		return gateway.SendError("BlueBubbles: failed to connect to server"), nil
	}
	result, sendErr := adapter.Send(ctx, chatID, message, "", nil)
	if err := adapter.Disconnect(ctx); err != nil {
		sendErr = err
	}
	if sendErr != nil {
		return fail(sendErr)
	}
	if !result.Success {
		return gateway.SendError("BlueBubbles send failed: " + pyrepr.Str(result.Error)), nil
	}
	return map[string]any{"success": true, "platform": "bluebubbles", "chat_id": chatID, "message_id": result.MessageID}, nil
}

// Print info for each line, nil printing a bare blank line
func info(lines ...*string) {
	for _, line := range lines {
		if line == nil {
			gateway.WizardPrintln("")
		} else {
			gateway.WizardCLI.PrintInfo(*line)
		}
	}
}

// Line pointer for the info helper
func ln(s string) *string { return &s }

// BlueBubbles iMessage wizard of icebath setup and icebath gateway setup
func InteractiveSetup(ctx context.Context) error {
	gateway.WizardCLI.PrintHeader("BlueBubbles (iMessage)")
	declined, err := gateway.PlatformSetup.DeclinesReconfigure(ctx, "BlueBubbles", "Reconfigure BlueBubbles?", "BLUEBUBBLES_SERVER_URL")
	if err != nil || declined {
		return err
	}
	info(ln("Connects Icebath to iMessage via BlueBubbles — a free, open-source"),
		ln("macOS server that bridges iMessage to any device."),
		ln("   Requires a Mac running BlueBubbles Server v1.0.0+"),
		ln("   Download: https://bluebubbles.app/"), nil,
		ln("In BlueBubbles Server → Settings → API, note your Server URL and Password."), nil)
	for _, field := range []struct {
		label, envVar, what string
		secret              bool
		transform           func(string) string
	}{
		{"BlueBubbles server URL (e.g. http://192.168.1.10:1234)", "BLUEBUBBLES_SERVER_URL", "Server URL", false,
			func(v string) string { return pystr.RstripChars(v, "/") }},
		{"BlueBubbles server password", "BLUEBUBBLES_PASSWORD", "Password", true, nil},
	} {
		saved, err := gateway.PlatformSetup.SavePrompted(ctx, field.envVar, field.label, gateway.SavePromptedOptions{
			Password: field.secret, Transform: field.transform, SkipMsg: field.what + " is required — skipping BlueBubbles setup"})
		if err != nil {
			return err
		}
		if saved == "" {
			return nil
		}
	}
	gateway.WizardCLI.PrintSuccess("BlueBubbles credentials saved")
	info(nil, ln("🔒 Security: Restrict who can message your bot"),
		ln("   Use iMessage addresses: email (user@icloud.com) or phone (+15551234567)"), nil)
	if _, err := gateway.PlatformSetup.PromptAllowlist(ctx, "BLUEBUBBLES_ALLOWED_USERS",
		"Allowed iMessage addresses (comma-separated, leave empty for open access)", "BlueBubbles allowlist configured",
		"⚠️  No allowlist set — anyone who can iMessage you can use the bot!", nil); err != nil {
		return err
	}
	info(nil, ln("📬 Home Channel: phone or email for cron job delivery and notifications."),
		ln("   You can also set this later with /set-home in your iMessage chat."))
	if _, err := gateway.PlatformSetup.SavePrompted(ctx, "BLUEBUBBLES_HOME_CHANNEL", "Home channel address (leave empty to set later)",
		gateway.SavePromptedOptions{}); err != nil {
		return err
	}
	info(nil, ln("Advanced settings (defaults are fine for most setups):"))
	if gateway.WizardCLI.PromptYesNo(ctx, "Configure webhook listener settings?", false) {
		if err := gateway.PlatformSetup.SavePort(ctx, "BLUEBUBBLES_WEBHOOK_PORT",
			gateway.WizardCLI.Prompt("Webhook listener port (default: 8645)", "", false), "8645"); err != nil {
			return err
		}
	}
	info(nil, ln("Requires the BlueBubbles Private API helper for typing indicators,"),
		ln("read receipts, and tapback reactions. Basic messaging works without it."),
		ln("   Install: https://docs.bluebubbles.app/helper-bundle/installation"))
	return nil
}

// Builds the adapter for a platform config, the factory the registry and the adapter class share
func newAdapter(ctx context.Context, cfg *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
	adapter, err := NewBlueBubblesAdapter(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return adapter, nil
}

// BlueBubbles platform plugin compiled into the binary under the bundled platforms/bluebubbles key
type blueBubblesPlatformPlugin struct{}

func (blueBubblesPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "bluebubbles-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/bluebubbles",
		Author: "NousResearch",
		Description: "BlueBubbles (Apple iMessage) gateway adapter for Icebath Agent. Talks to a local BlueBubbles macOS server: " +
			"outbound sends over its REST API, inbound messages via a registered webhook listener. Supports media attachments, " +
			"typing indicators, read receipts and tapbacks (Private API helper), group wake-word gating, and home-channel cron delivery.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "BLUEBUBBLES_SERVER_URL", Description: "BlueBubbles server URL (e.g. http://192.168.1.10:1234)"},
			{Name: "BLUEBUBBLES_PASSWORD", Description: "BlueBubbles server password (BlueBubbles Server → Settings → API)", Secret: true}}}
}

// Registers the BlueBubbles platform entry with its factory, setup, standalone sender and card
func (blueBubblesPlatformPlugin) Register(ctx *plugins.Context) error {
	extra := map[string]any{
		"dashboard_description":  "Use Icebath through iMessage via a BlueBubbles server.",
		"docs_url":               "https://bluebubbles.app/",
		"dashboard_env_vars":     []string{"BLUEBUBBLES_SERVER_URL", "BLUEBUBBLES_PASSWORD", "BLUEBUBBLES_ALLOWED_USERS"},
		"is_connected":           isConnected,
		"config_connected_fn":    gateway.ConfigConnectedExtra("server_url", "password"),
		"setup_fn":               InteractiveSetup,
		"cron_deliver_env_var":   "BLUEBUBBLES_HOME_CHANNEL",
		"standalone_sender_fn":   standaloneSend,
		"max_message_length":     0,
		"pii_safe":               true,
		"emoji":                  "💬",
		"allow_update_command":   true,
		"platform_hint":          platformHint,
		"display_tier":           "low",
		"env_overrides":          envOverrides,
		"env_enable_credentials": envEnableCredentials,
	}
	for k, v := range authz {
		extra[k] = v
	}
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "bluebubbles", Label: "BlueBubbles (iMessage)",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return newAdapter(fctx, cfg)
		}),
		Check:       checkRequirements,
		RequiredEnv: []string{"BLUEBUBBLES_SERVER_URL", "BLUEBUBBLES_PASSWORD"},
		InstallHint: installHint,
		Extra:       extra,
	})
	return err
}

func init() {
	gateway.RegisterAdapterModule(&gateway.AdapterClass{Name: "BlueBubblesAdapter", New: newAdapter, Traits: adapterTraits()})
	plugins.RegisterBundled(blueBubblesPlatformPlugin{})
}
