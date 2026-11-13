package telegram

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Telegram REST delivery of the send message tool the process wiring installs for out of process sends
var SendTelegramREST func(ctx context.Context, token any, chatID, message string, mediaFiles []gateway.MediaRef, threadID *string,
	disableLinkPreviews, forceDocument bool) (map[string]any, error)

// Adapter with the notification mode of the environment or display config applied
func buildAdapter(ctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
	a := NewTelegramAdapter(ctx, cfg)
	a.notificationsMode = resolveNotificationsMode(ctx)
	return a, nil
}

// Connected when a bot token is configured in the platform config or the env file
func isConnected(ctx context.Context, cfg *gateway.PlatformConfig) (bool, error) {
	token := ""
	if cfg != nil {
		token = tokenText(cfg.Token)
	}
	if token == "" {
		value, _, err := config.GetEnvValue(ctx, home.Dir(ctx), "TELEGRAM_BOT_TOKEN")
		if err != nil {
			return false, err
		}
		token = value
	}
	return pystr.Strip(token) != "", nil
}

// Out of process delivery so deliver=telegram cron jobs succeed without the gateway, through the REST sender
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	if SendTelegramREST == nil {
		panic("telegram.SendTelegramREST unassigned: the process wiring installs the send message tool Telegram sender")
	}
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	var token any
	if pconfig != nil {
		token = pconfig.Token
	}
	if tokenText(token) == "" {
		token = provider.GetSecretStr(ctx, "TELEGRAM_BOT_TOKEN", "")
	}
	disablePreviews := pconfig != nil && len(pconfig.Extra) > 0 && pyTruthy(pconfig.Extra["disable_link_previews"])
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	var threadID *string
	if v, ok := kwargs["thread_id"].(string); ok {
		threadID = &v
	}
	media, _ := kwargs["media_files"].([]gateway.MediaRef)
	forceDocument, _ := kwargs["force_document"].(bool)
	return SendTelegramREST(ctx, token, chatID, message, media, threadID, disablePreviews, forceDocument)
}

// Configures Telegram credentials and the allowlist through the CLI setup wizard
func interactiveSetup(ctx context.Context) error { return gateway.SetupTelegram(ctx) }

// Bundled Telegram platform plugin registering the adapter with the gateway
type telegramPlugin struct{}

func (telegramPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "telegram-platform", Key: "platforms/telegram", Kind: "platform", Version: "1.0.0", Author: "NousResearch",
		Description: "Telegram gateway adapter for Icebath Agent. Connects to Telegram via go-telegram/bot and relays messages between " +
			"Telegram chats/groups/topics and the Icebath agent. Supports threads/topics, streaming edits, native media, inline keyboards, " +
			"slash commands, fallback network transport (direct-IP failover), notification modes, mention gating, and per-user/chat allowlists.",
		RequiresEnv: []plugins.EnvSpec{{Name: "TELEGRAM_BOT_TOKEN", Description: "Telegram bot token from @BotFather", URL: "https://t.me/BotFather",
			Secret: true}}}
}

// Registers the Telegram platform with its sender, YAML bridge, setup, dashboard card and access policy declarations
func (telegramPlugin) Register(ctx *plugins.Context) error {
	extra := map[string]any{
		"ensure_deps_fn":         func(context.Context) (bool, error) { return true, nil },
		"is_connected":           isConnected,
		"setup_fn":               interactiveSetup,
		"apply_yaml_config_fn":   applyYAMLConfig,
		"cron_deliver_env_var":   "TELEGRAM_HOME_CHANNEL",
		"standalone_sender_fn":   standaloneSend,
		"max_message_length":     maxMessageLength,
		"emoji":                  "✈️",
		"allow_update_command":   true,
		"platform_hint":          platformHint,
		"display_tier":           "high",
		"display_overrides":      map[string]any{"tool_progress": "off", "busy_ack_detail": false},
		"target_ref_syntax_fn":   ParseSendTarget,
		"env_overrides":          envOverrides,
		"env_enable_credentials": envEnableCredentials,
	}
	for k, v := range authz {
		extra[k] = v
	}
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{Name: "telegram", Label: "Telegram", AdapterFactory: gateway.AdapterFactory(buildAdapter),
		Check: func() bool { return true }, RequiredEnv: []string{"TELEGRAM_BOT_TOKEN"},
		InstallHint: i18n.T(ctx.HomeContext(), "platform.telegram.install_hint", "", nil), Extra: extra})
	return err
}

// Adds the Telegram platform plugin to the compiled in set
func init() { plugins.RegisterBundled(telegramPlugin{}) }
