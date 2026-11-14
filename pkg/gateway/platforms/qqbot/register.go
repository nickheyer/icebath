package qqbot

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Both credentials present in the home env file
var envConfigured = gateway.EnvIsConnected("QQ_APP_ID", "QQ_CLIENT_SECRET")

// Both credentials from the config extra, else from the profile env file
func isConnected(ctx context.Context, pconfig *gateway.PlatformConfig) (bool, error) {
	if pconfig != nil && textOf(pconfig.Extra["app_id"]) != "" && textOf(pconfig.Extra["client_secret"]) != "" {
		return true, nil
	}
	return envConfigured(ctx, pconfig)
}

// Out of process sender that delivers through the REST API so cron works without a live gateway
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	mediaFiles, _ := kwargs["media_files"].([]gateway.MediaRef)
	caption, _ := kwargs["caption"].(string)
	return sendQQBot(ctx, pconfig, chatID, message, mediaFiles, caption)
}

// QR registration the wizard runs, a seam tests replace
var qrRegisterFn = qrRegister

// Interactive setup by QR scan or manual App ID and App Secret, then the DM policy and home channel
func setupQQBot(ctx context.Context) error {
	cli, cfg, setup, wizard := gateway.WizardCLI, gateway.WizardConfig, gateway.WizardSetup, gateway.WizardGateway
	wizard.PrintSetupHeader("🐧 QQ Bot")
	proceed, err := wizard.ConfirmReconfigure(ctx, "QQ Bot", "QQ_APP_ID", "QQ_CLIENT_SECRET")
	if err != nil || !proceed {
		return err
	}
	gateway.WizardPrintln("")
	var credentials *onboardCredentials
	if setup.PromptChoice("  How would you like to set up QQ Bot?", []string{"Scan QR code to add bot automatically (recommended)",
		"Enter existing App ID and App Secret manually"}, 0) == 0 {
		credentials, err = qrRegisterFn(ctx, 600)
		if errors.Is(err, gateway.ErrKeyboardInterrupt) {
			gateway.WizardPrintln("")
			cli.PrintWarning("  QQ Bot setup cancelled.")
			return nil
		}
		if err != nil {
			return err
		}
		if credentials == nil {
			cli.PrintInfo("  QR setup did not complete. Continuing with manual input.")
		}
	}
	if credentials == nil {
		gateway.WizardPrintln("")
		cli.PrintInfo("  Go to https://q.qq.com to register a QQ Bot application.")
		cli.PrintInfo("  Note your App ID and App Secret from the application page.")
		gateway.WizardPrintln("")
		appID := setup.Prompt("  App ID", "", false)
		if appID == "" {
			cli.PrintWarning("  Skipped — QQ Bot won't work without an App ID.")
			return nil
		}
		appSecret := setup.Prompt("  App Secret", "", true)
		if appSecret == "" {
			cli.PrintWarning("  Skipped — QQ Bot won't work without an App Secret.")
			return nil
		}
		credentials = &onboardCredentials{appID: pystr.Strip(appID), clientSecret: pystr.Strip(appSecret)}
	}
	if err := cfg.SaveEnvValue(ctx, "QQ_APP_ID", credentials.appID); err != nil {
		return err
	}
	if err := cfg.SaveEnvValue(ctx, "QQ_CLIENT_SECRET", credentials.clientSecret); err != nil {
		return err
	}
	userOpenID := credentials.userOpenID
	gateway.WizardPrintln("")
	switch setup.PromptChoice("  How should direct messages be authorized?", []string{"Use DM pairing approval (recommended)",
		"Allow all direct messages", "Only allow listed user OpenIDs"}, 0) {
	case 0:
		if err := cfg.SaveEnvValue(ctx, "QQ_ALLOW_ALL_USERS", "false"); err != nil {
			return err
		}
		allowed := ""
		if userOpenID != "" {
			gateway.WizardPrintln("")
			if setup.PromptYesNo("  Add yourself ("+userOpenID+") to the allow list?", true) {
				allowed = userOpenID
				cli.PrintSuccess("  Allow list set to " + userOpenID)
			}
		}
		if err := cfg.SaveEnvValue(ctx, "QQ_ALLOWED_USERS", allowed); err != nil {
			return err
		}
		cli.PrintSuccess("  DM pairing enabled.")
		cli.PrintInfo("  Unknown users can request access; approve with `icebath pairing approve`.")
	case 1:
		if err := wizard.SaveEnvValues(ctx, gateway.EnvAssignment{Name: "QQ_ALLOW_ALL_USERS", Value: "true"},
			gateway.EnvAssignment{Name: "QQ_ALLOWED_USERS", Value: ""}); err != nil {
			return err
		}
		cli.PrintWarning("  Open DM access enabled for QQ Bot.")
	default:
		allowlist := wizard.PromptCSV("  Allowed user OpenIDs (comma-separated)", userOpenID)
		if err := wizard.SaveEnvValues(ctx, gateway.EnvAssignment{Name: "QQ_ALLOW_ALL_USERS", Value: "false"},
			gateway.EnvAssignment{Name: "QQ_ALLOWED_USERS", Value: allowlist}); err != nil {
			return err
		}
		cli.PrintSuccess("  Allowlist saved.")
	}
	gateway.WizardPrintln("")
	if userOpenID != "" {
		if err := wizard.OfferHomeChannel(ctx, "QQBOT_HOME_CHANNEL", userOpenID, "your QQ user ID"); err != nil {
			return err
		}
	} else if homeChannel := setup.Prompt("  Home channel OpenID (for cron/notifications, or empty)", "", false); homeChannel != "" {
		if err := cfg.SaveEnvValue(ctx, "QQBOT_HOME_CHANNEL", pystr.Strip(homeChannel)); err != nil {
			return err
		}
		cli.PrintSuccess("  Home channel set to " + pystr.Strip(homeChannel))
	}
	gateway.WizardPrintln("")
	cli.PrintSuccess("🐧 QQ Bot configured!")
	cli.PrintInfo("  App ID: " + credentials.appID)
	return nil
}

// System prompt platform hint, cached prompt bytes kept stable
const platformHint = "You are on QQ, a popular Chinese messaging platform. QQ supports markdown formatting " +
	"and emoji. You can send media files natively: include MEDIA:/absolute/path/to/file in " +
	"your response. Images are sent as native photos, and other files arrive as downloadable documents."

// Builds the adapter for a platform config, the factory the registry and the adapter class share
func newAdapter(ctx context.Context, cfg *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
	adapter, err := NewQQAdapter(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return adapter, nil
}

// QQ Bot platform plugin compiled into the binary under the bundled platforms/qqbot key
type qqbotPlatformPlugin struct{}

func (qqbotPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "qqbot-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/qqbot",
		Author:      "Nous Research",
		Description: "QQ Bot gateway adapter (Official QQ Bot API v2). WebSocket gateway for inbound events, REST for outbound text and media.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "QQ_APP_ID", Description: "QQ Bot App ID from QQ Open Platform (q.qq.com)", URL: "https://q.qq.com"},
			{Name: "QQ_CLIENT_SECRET", Description: "QQ Bot Client Secret from QQ Open Platform", Secret: true}}}
}

// Registers the QQ Bot platform entry with its factory, checks, setup and standalone sender
func (qqbotPlatformPlugin) Register(ctx *plugins.Context) error {
	extra := map[string]any{
		"standalone_send":        &gateway.StandaloneSendPolicy{Label: "QQ Bot", Media: true, Captionable: true, TextDirect: true},
		"dashboard_description":  "Connect Icebath to a QQ Bot from the QQ Open Platform.",
		"docs_url":               "https://q.qq.com",
		"dashboard_env_vars":     []string{"QQ_APP_ID", "QQ_CLIENT_SECRET", "QQ_ALLOWED_USERS"},
		"is_connected":           isConnected,
		"config_connected_fn":    gateway.ConfigConnectedExtra("app_id", "client_secret"),
		"setup_fn":               setupQQBot,
		"emoji":                  "🐧",
		"cron_deliver_env_var":   "QQBOT_HOME_CHANNEL",
		"standalone_sender_fn":   standaloneSend,
		"platform_hint":          platformHint,
		"env_overrides":          envOverrides,
		"env_enable_credentials": envEnableCredentials,
	}
	for k, v := range authz {
		extra[k] = v
	}
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "qqbot", Label: "QQ Bot",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) { return newAdapter(fctx, cfg) }),
		Check:          checkQQRequirements,
		RequiredEnv:    []string{"QQ_APP_ID", "QQ_CLIENT_SECRET"},
		InstallHint:    "QQBot: aiohttp/httpx missing or QQ_APP_ID/QQ_CLIENT_SECRET not configured",
		Extra:          extra,
	})
	return err
}

var (
	_ gateway.PlatformAdapter    = (*QQAdapter)(nil)
	_ gateway.UpdatePromptSender = (*QQAdapter)(nil)
)

func init() {
	gateway.RegisterAdapterModule(&gateway.AdapterClass{Name: "QQAdapter", New: newAdapter, Traits: adapterTraits()})
	plugins.RegisterBundled(qqbotPlatformPlugin{})
}
