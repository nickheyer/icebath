package wecom

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/wecom/callback"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
)

const installHint = "Run `icebath setup` to install WeCom support."

// Model facing description of WeCom rendering and file sends, kept byte stable for the cached prompt
const platformHint = "You are on WeCom (企业微信). Markdown is supported. " +
	"You can send files natively: write MEDIA:/absolute/path/to/file in your response. " +
	"Images (.jpg, .png, .webp) send as photos (≤10 MB), other files as documents (≤20 MB), videos " +
	"(.mp4) play inline. Voice messages must be AMR — other audio formats send as file attachments. Image " +
	"URLs via ![alt](url) are downloaded and sent as photos. Never claim you lack file-sending."

// Dependency gate of both platforms, met because the websocket, HTTP and crypto code is compiled in
func checkRequirements() bool { return true }

// Whether the AI bot config carries a bot id
func isConnected(cfg *gateway.PlatformConfig) bool { return cfg != nil && isSet(cfg.Extra["bot_id"]) }

// Whether the callback config carries a corp id or a multi app list
func callbackIsConnected(cfg *gateway.PlatformConfig) bool {
	return cfg != nil && (isSet(cfg.Extra["corp_id"]) || isSet(cfg.Extra["apps"]))
}

// Live AI bot adapter of the active profile in the in process gateway, nil when none runs
func liveAdapter(ctx context.Context) gateway.PlatformAdapter {
	runner, ok := gateway.CurrentGatewayRunner().(gateway.AuthorizationAdapterRunner)
	if !ok || runner == nil {
		return nil
	}
	profile := profiles.GetActiveProfileName(ctx)
	return runner.AuthorizationAdapter(ctx, gateway.PlatformWeCom, &profile)
}

// Send through an adapter as a standalone sender result
func sendVia(ctx context.Context, adapter gateway.PlatformAdapter, chatID, message string, live bool) (map[string]any, error) {
	result, err := adapter.Send(ctx, chatID, message, "", nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if live {
			return gateway.SendError("WeCom live adapter send failed: " + err.Error()), nil
		}
		return gateway.SendError("WeCom send failed: " + err.Error()), nil
	}
	if result.Success {
		return map[string]any{"success": true, "platform": "wecom", "chat_id": chatID, "message_id": result.MessageID}, nil
	}
	return gateway.SendError(fmt.Sprintf("WeCom send failed: %v", result.Error)), nil
}

// Out of gateway text delivery, reusing the live adapter since a second websocket would kick the bot's first one
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	if live := liveAdapter(ctx); live != nil {
		return sendVia(ctx, live, chatID, message, true)
	}
	adapter, err := NewWeComAdapter(ctx, pconfig)
	if err != nil {
		return gateway.SendError("WeCom send failed: " + err.Error()), nil
	}
	connected, err := adapter.Connect(ctx, false)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return gateway.SendError("WeCom send failed: " + err.Error()), nil
	}
	if !connected {
		reason := "unknown error"
		if msg := adapter.FatalErrorMessage(); msg != nil && *msg != "" {
			reason = *msg
		}
		return gateway.SendError("WeCom: failed to connect - " + reason), nil
	}
	defer func() { _ = adapter.Disconnect(context.WithoutCancel(ctx)) }()
	return sendVia(ctx, adapter, chatID, message, false)
}

// Builds the AI bot adapter as a platform adapter, nil on a config error
func newAdapter(ctx context.Context, cfg *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
	adapter, err := NewWeComAdapter(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return adapter, nil
}

// WeCom platform plugin compiled into the binary under the bundled platforms/wecom key
type weComPlatformPlugin struct{}

func (weComPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "wecom-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/wecom",
		Author: "heyer.app",
		Description: "WeCom / Enterprise WeChat gateway adapter for Icebath Agent. Registers two platforms: wecom (Smart Robot over " +
			"WebSocket) and wecom_callback (self-built apps over an HTTP callback endpoint with AES message crypto). Relays " +
			"messages between WeCom chats and the Icebath agent.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "WECOM_BOT_ID", Description: "WeCom Smart Robot bot ID"},
			{Name: "WECOM_SECRET", Description: "WeCom Smart Robot secret", Secret: true}}}
}

// Registers the AI bot and callback platform entries with their factories, gates, env bridges, setup and sender
func (weComPlatformPlugin) Register(ctx *plugins.Context) error {
	extra := map[string]any{
		"target_ref_syntax_fn":   gateway.ParseNonempty,
		"standalone_send":        &gateway.StandaloneSendPolicy{TextDirect: true},
		"dashboard_label":        "WeCom (group bot)",
		"dashboard_description":  "Send-only WeCom group bot via webhook.",
		"docs_url":               "https://developer.work.weixin.qq.com/document/path/91770",
		"dashboard_env_vars":     []string{"WECOM_BOT_ID", "WECOM_SECRET"},
		"dashboard_required_env": []string{"WECOM_BOT_ID"},
		"is_connected":           isConnected,
		"setup_fn":               InteractiveSetup,
		"cron_deliver_env_var":   "WECOM_HOME_CHANNEL",
		"standalone_sender_fn":   standaloneSend,
		"max_message_length":     maxMessageLength,
		"emoji":                  "💼",
		"allow_update_command":   true,
		"platform_hint":          platformHint,
		"display_tier":           "low",
		"display_overrides":      map[string]any{"streaming": true},
		"env_overrides":          envOverrides,
		"env_enable_credentials": envEnableCredentials,
	}
	for k, v := range authz {
		extra[k] = v
	}
	if _, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "wecom", Label: "WeCom (Enterprise WeChat)",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return newAdapter(fctx, cfg)
		}),
		Check:          checkRequirements,
		ValidateConfig: isConnected,
		RequiredEnv:    []string{"WECOM_BOT_ID", "WECOM_SECRET"},
		InstallHint:    installHint,
		Extra:          extra,
	}); err != nil {
		return err
	}
	callbackExtra := map[string]any{
		"dashboard_label":       "WeCom (app)",
		"dashboard_description": "Two-way WeCom integration via callback app.",
		"docs_url":              "https://developer.work.weixin.qq.com/document/path/90930",
		"dashboard_env_vars": []string{"WECOM_CALLBACK_CORP_ID", "WECOM_CALLBACK_CORP_SECRET", "WECOM_CALLBACK_AGENT_ID",
			"WECOM_CALLBACK_TOKEN", "WECOM_CALLBACK_ENCODING_AES_KEY"},
		"dashboard_required_env": []string{"WECOM_CALLBACK_CORP_ID", "WECOM_CALLBACK_CORP_SECRET", "WECOM_CALLBACK_AGENT_ID"},
		"is_connected":           callbackIsConnected,
		"emoji":                  "💼",
		"allow_update_command":   true,
		"display_tier":           "low",
		"env_overrides":          callbackEnvOverrides,
		"env_enable_credentials": callbackEnvEnableCredentials,
	}
	for k, v := range callbackAuthz {
		callbackExtra[k] = v
	}
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "wecom_callback", Label: "WeCom Callback (self-built apps)",
		AdapterFactory: gateway.AdapterFactory(callback.Factory),
		Check:          checkRequirements,
		ValidateConfig: callbackIsConnected,
		RequiredEnv:    []string{"WECOM_CALLBACK_CORP_ID", "WECOM_CALLBACK_CORP_SECRET"},
		InstallHint:    installHint,
		Extra:          callbackExtra,
	})
	return err
}

func init() {
	gateway.RegisterAdapterModule(&gateway.AdapterClass{Name: "WeComAdapter", New: newAdapter, Traits: adapterTraits()})
	plugins.RegisterBundled(weComPlatformPlugin{})
}
