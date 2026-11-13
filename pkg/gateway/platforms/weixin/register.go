package weixin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Model facing description of Weixin rendering and media delivery, kept byte stable for the cached prompt
const platformHint = "You are on Weixin/WeChat. Markdown formatting is supported, so you may use it when it improves readability, " +
	"but keep the message compact and chat-friendly. You can send media files natively: include " +
	"MEDIA:/absolute/path/to/file in your response. Images are sent as native photos, videos play inline when " +
	"supported, and other files arrive as downloadable documents. You can also include image URLs in markdown " +
	"format ![alt](url) and they will be downloaded and sent as native media when possible."

// Explicit send target forms, WeChat ids, chatrooms and the file helper
const sendTargetPattern = `^\s*((?:wxid|gh|v\d+|wm|wb)_[A-Za-z0-9_-]+|[A-Za-z0-9._-]+@chatroom|filehelper)\s*$`

// Image extensions a standalone send delivers as native photos
var directImageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".bmp": true}

// Connected adapters of this process keyed by bot token, reused by standalone sends
var live = struct {
	sync.Mutex
	adapters map[string]*Adapter
}{adapters: map[string]*Adapter{}}

// Records a connected adapter under its token
func registerLive(a *Adapter) {
	live.Lock()
	live.adapters[a.token] = a
	live.Unlock()
}

// Removes the adapter from the live set when it still owns its token
func unregisterLive(a *Adapter) {
	live.Lock()
	if live.adapters[a.token] == a {
		delete(live.adapters, a.token)
	}
	live.Unlock()
}

// Connected adapter of a token, nil when none
func liveAdapter(token string) *Adapter {
	live.Lock()
	defer live.Unlock()
	if a := live.adapters[token]; a != nil && a.IsConnected() {
		return a
	}
	return nil
}

// Adapter settings map of a platform config, empty for a nil config
func extraOf(cfg *gateway.PlatformConfig) map[string]any {
	if cfg == nil || cfg.Extra == nil {
		return map[string]any{}
	}
	return cfg.Extra
}

// Sends the text and then each file, images as photos and the rest as documents
func deliverDirect(ctx context.Context, a *Adapter, chatID, message string, media []gateway.MediaRef, contextToken string) (map[string]any, error) {
	var last *gateway.SendResult
	if cleaned := a.FormatMessage(message); cleaned != "" {
		result, err := a.Send(ctx, chatID, cleaned, "", nil)
		if err != nil {
			return nil, err
		}
		if !result.Success {
			return map[string]any{"error": fmt.Sprintf("Weixin send failed: %v", result.Error)}, nil
		}
		last = &result
	}
	for _, m := range media {
		var result gateway.SendResult
		var err error
		if directImageExts[strings.ToLower(filepath.Ext(m.Path))] {
			result, err = a.SendImageFile(ctx, chatID, m.Path, "", "", nil, nil)
		} else {
			result, err = a.SendDocument(ctx, chatID, m.Path, "", "", "", nil, nil)
		}
		if err != nil {
			return nil, err
		}
		if !result.Success {
			return map[string]any{"error": fmt.Sprintf("Weixin media send failed: %v", result.Error)}, nil
		}
		last = &result
	}
	var messageID any
	if last != nil {
		messageID = last.MessageID
	}
	return map[string]any{"success": true, "platform": "weixin", "chat_id": chatID, "message_id": messageID,
		"context_token_used": contextToken != ""}, nil
}

// One shot send through the live adapter of the token, else through an unconnected adapter with its own client
func sendDirect(ctx context.Context, pconfig *gateway.PlatformConfig, chatID, message string, media []gateway.MediaRef) (map[string]any, error) {
	extra := extraOf(pconfig)
	accountID, err := setting(ctx, extra, "account_id", "")
	if err != nil {
		return nil, err
	}
	baseURL, err := setting(ctx, extra, "base_url", defaultBaseURL)
	if err != nil {
		return nil, err
	}
	cdnBaseURL, err := setting(ctx, extra, "cdn_base_url", defaultCDNBaseURL)
	if err != nil {
		return nil, err
	}
	token, err := resolveToken(ctx, pconfig, extra)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return map[string]any{"error": "Weixin token missing. Configure WEIXIN_TOKEN or platforms.weixin.token."}, nil
	}
	if accountID == "" {
		return map[string]any{"error": "Weixin account ID missing. Configure WEIXIN_ACCOUNT_ID or platforms.weixin.extra.account_id."}, nil
	}
	store := newContextTokenStore(home.Dir(ctx))
	store.restore(accountID)
	contextToken := store.get(accountID, chatID)
	if a := liveAdapter(token); a != nil {
		return deliverDirect(ctx, a, chatID, message, media, contextToken)
	}
	cfg := gateway.NewPlatformConfig()
	if pconfig != nil {
		copied := *pconfig
		cfg = &copied
	}
	cfg.Enabled, cfg.Token = true, token
	merged := map[string]any{}
	for k, v := range extra {
		merged[k] = v
	}
	merged["account_id"], merged["base_url"], merged["cdn_base_url"] = accountID, strings.TrimRight(baseURL, "/"), strings.TrimRight(cdnBaseURL, "/")
	cfg.Extra = merged
	a, err := NewAdapter(ctx, cfg)
	if err != nil {
		return nil, err
	}
	client := newHTTPClient(ctx)
	defer client.CloseIdleConnections()
	a.mu.Lock()
	a.sendClient = client
	a.mu.Unlock()
	a.tokens = store
	return deliverDirect(ctx, a, chatID, message, media, contextToken)
}

// Out of process sender for send_message and cron, native text and media through iLink
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	media, _ := kwargs["media_files"].([]gateway.MediaRef)
	result, err := sendDirect(ctx, pconfig, chatID, message, media)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		slog.Debug("Weixin standalone send failed", "error", err)
		return gateway.SendError("Weixin send failed: " + err.Error()), nil
	}
	return result, nil
}

// Whether account id and token resolve from the config or the scoped env, else from the home .env
func isConnected(ctx context.Context, cfg *gateway.PlatformConfig) (bool, error) {
	extra := extraOf(cfg)
	account, err := setting(ctx, extra, "account_id", "")
	if err != nil {
		return false, err
	}
	token, err := resolveToken(ctx, cfg, extra)
	if err != nil {
		return false, err
	}
	if account != "" && token != "" {
		return true, nil
	}
	return gateway.EnvIsConnected("WEIXIN_ACCOUNT_ID", "WEIXIN_TOKEN")(ctx, cfg)
}

// Whether the merged config carries both the account id and a token
func configConnected(cfg *gateway.PlatformConfig) bool {
	if cfg == nil {
		return false
	}
	extra := extraOf(cfg)
	return present(extra["account_id"]) && (present(cfg.Token) || present(extra["token"]))
}

// Builds the adapter for a platform config, the factory the registry and the adapter class share
func newAdapter(ctx context.Context, cfg *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
	a, err := NewAdapter(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// Weixin platform plugin compiled into the binary under the bundled platforms/weixin key
type weixinPlatformPlugin struct{}

func (weixinPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "weixin-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/weixin",
		Author: "heyer.app",
		Description: "Weixin (personal WeChat) gateway adapter for Icebath over Tencent's iLink Bot API. Long-polls getupdates for " +
			"inbound messages, echoes each peer's context_token on replies, and moves media through the AES-128-ECB encrypted " +
			"CDN. `icebath gateway setup` performs the iLink QR login.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "WEIXIN_ACCOUNT_ID", Description: "iLink Bot account ID obtained through QR login in icebath gateway setup"},
			{Name: "WEIXIN_TOKEN", Description: "iLink Bot token obtained through QR login in icebath gateway setup", Secret: true}}}
}

// Registers the Weixin platform entry with its factory, setup, standalone sender, target syntax and hint
func (weixinPlatformPlugin) Register(ctx *plugins.Context) error {
	extra := map[string]any{
		"target_ref_syntax_fn":   gateway.RegexGroupsParser(sendTargetPattern, false),
		"standalone_send":        &gateway.StandaloneSendPolicy{Label: "Weixin", Media: true},
		"dashboard_label":        "Weixin / WeChat (Personal)",
		"dashboard_description":  "Connect a personal WeChat account through Tencent's iLink Bot API.",
		"docs_url":               "https://icebath.app/docs/user-guide/messaging/weixin/",
		"dashboard_env_vars":     []string{"WEIXIN_ACCOUNT_ID", "WEIXIN_TOKEN", "WEIXIN_BASE_URL"},
		"is_connected":           isConnected,
		"config_connected_fn":    configConnected,
		"setup_fn":               interactiveSetup,
		"cron_deliver_env_var":   "WEIXIN_HOME_CHANNEL",
		"standalone_sender_fn":   standaloneSend,
		"max_message_length":     0,
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
		Name: "weixin", Label: "Weixin / WeChat",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return newAdapter(fctx, cfg)
		}),
		Check:       func() bool { return true },
		RequiredEnv: []string{"WEIXIN_ACCOUNT_ID", "WEIXIN_TOKEN"},
		Extra:       extra,
	})
	return err
}

func init() {
	gateway.RegisterAdapterModule(&gateway.AdapterClass{Name: "WeixinAdapter", New: newAdapter, Traits: adapterTraits()})
	plugins.RegisterBundled(weixinPlatformPlugin{})
}
