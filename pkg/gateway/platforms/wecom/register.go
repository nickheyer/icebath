package wecom

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Sends through an adapter and wraps the outcome in the standalone sender envelope
func sendVia(ctx context.Context, adapter gateway.PlatformAdapter, chatID, message string, live bool) (map[string]any, error) {
	result, err := adapter.Send(ctx, chatID, message, "", nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if live {
			return gateway.SendError("WeCom live adapter send failed: " + errText(err)), nil
		}
		return gateway.SendError("WeCom send failed: " + errText(err)), nil
	}
	if result.Success {
		return map[string]any{"success": true, "platform": "wecom", "chat_id": chatID, "message_id": result.MessageID}, nil
	}
	return gateway.SendError("WeCom send failed: " + pyrepr.Str(result.Error)), nil
}

// Reuses the active profile's live adapter in process, else connects one ephemeral adapter for the send
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	if lookup := gateway.SendMessageSendersLiveAdapter; lookup != nil {
		if live := lookup(ctx, gateway.PlatformWeCom); live != nil {
			return sendVia(ctx, live, chatID, message, true)
		}
	}
	fail := func(err error) (map[string]any, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return gateway.SendError("WeCom send failed: " + errText(err)), nil
	}
	adapter, err := NewWeComAdapter(ctx, pconfig)
	if err != nil {
		return fail(err)
	}
	connected, err := adapter.Connect(ctx, false)
	if err != nil {
		return fail(err)
	}
	if !connected {
		reason := "unknown error"
		if msg := adapter.FatalErrorMessage(); msg != nil && *msg != "" {
			reason = *msg
		}
		return gateway.SendError("WeCom: failed to connect - " + reason), nil
	}
	result, sendErr := sendVia(ctx, adapter, chatID, message, false)
	if err := adapter.Disconnect(ctx); err != nil {
		return fail(err)
	}
	return result, sendErr
}

// Connected means a bot id in the config extra
func isConnected(cfg *gateway.PlatformConfig) bool {
	return cfg != nil && pyrepr.Truthy(cfg.Extra["bot_id"])
}

// Callback mode is connected with a corp id or a multi app apps block
func callbackIsConnected(cfg *gateway.PlatformConfig) bool {
	return cfg != nil && (pyrepr.Truthy(cfg.Extra["corp_id"]) || pyrepr.Truthy(cfg.Extra["apps"]))
}

// Builds the websocket adapter for a platform config
func newAdapter(ctx context.Context, cfg *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
	adapter, err := NewWeComAdapter(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return adapter, nil
}

// Builds the callback mode adapter for a platform config
func buildCallbackAdapter(ctx context.Context, cfg *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
	adapter, err := NewWecomCallbackAdapter(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return adapter, nil
}

// System prompt platform hint, cached prompt bytes kept stable
const platformHint = "You are on WeCom (企业微信). Markdown is supported. " +
	"You can send files natively: write MEDIA:/absolute/path/to/file in your response. " +
	"Images (.jpg, .png, .webp) send as photos (≤10 MB), other files as documents (≤20 MB), videos " +
	"(.mp4) play inline. Voice messages must be AMR — other audio formats send as file attachments. Image " +
	"URLs via ![alt](url) are downloaded and sent as photos. Never claim you lack file-sending."

const installHint = "Run `icebath setup` to install WeCom support."

// WeCom platform plugin compiled into the binary under the bundled platforms/wecom key
type wecomPlatformPlugin struct{}

func (wecomPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "wecom-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/wecom",
		Author: "NousResearch",
		Description: "WeCom / Enterprise WeChat gateway adapter for Icebath Agent. Registers two platforms: ``wecom`` (Smart Robot over WebSocket) " +
			"and ``wecom_callback`` (self-built apps over an HTTP callback endpoint with AES message crypto). " +
			"Relays messages between WeCom chats and the Icebath agent.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "WECOM_BOT_ID", Description: "WeCom Smart Robot bot ID"},
			{Name: "WECOM_SECRET", Description: "WeCom Smart Robot secret", Secret: true}}}
}

// Registers the websocket bot platform and the callback app platform with their factories, checks and senders
func (wecomPlatformPlugin) Register(ctx *plugins.Context) error {
	extra := map[string]any{
		"target_ref_syntax_fn":   gateway.ParseNonempty,
		"standalone_send":        &gateway.StandaloneSendPolicy{TextDirect: true},
		"dashboard_label":        "WeCom (group bot)",
		"dashboard_description":  "Send-only WeCom group bot via webhook.",
		"docs_url":               "https://developer.work.weixin.qq.com/document/path/91770",
		"dashboard_env_vars":     []string{"WECOM_BOT_ID", "WECOM_SECRET"},
		"dashboard_required_env": []string{"WECOM_BOT_ID"},
		"is_connected":           isConnected,
		"validate_config":        isConnected,
		"setup_fn":               InteractiveSetup,
		"cron_deliver_env_var":   "WECOM_HOME_CHANNEL",
		"standalone_sender_fn":   standaloneSend,
		"max_message_length":     4000,
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
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) { return newAdapter(fctx, cfg) }),
		Check:          checkWeComRequirements,
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
		"ensure_deps_fn":         ensureWeComCallbackRequirements,
		"is_connected":           callbackIsConnected,
		"validate_config":        callbackIsConnected,
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
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) { return buildCallbackAdapter(fctx, cfg) }),
		Check:          checkWeComCallbackRequirements,
		RequiredEnv:    []string{"WECOM_CALLBACK_CORP_ID", "WECOM_CALLBACK_CORP_SECRET"},
		InstallHint:    installHint,
		Extra:          callbackExtra,
	})
	if err != nil {
		return fmt.Errorf("wecom_callback: %w", err)
	}
	return nil
}

var (
	_ gateway.PlatformAdapter     = (*WeComAdapter)(nil)
	_ gateway.NativeStreamAdapter = (*WeComAdapter)(nil)
	_ StreamHost                  = (*WeComAdapter)(nil)
	_ gateway.PlatformAdapter     = (*WecomCallbackAdapter)(nil)
)

func init() {
	gateway.RegisterAdapterModule(
		&gateway.AdapterClass{Name: "WeComAdapter", New: newAdapter, Traits: adapterTraits()},
		&gateway.AdapterClass{Name: "WecomCallbackAdapter", New: buildCallbackAdapter, Traits: callbackAdapterTraits()})
	plugins.RegisterBundled(wecomPlatformPlugin{})
}
