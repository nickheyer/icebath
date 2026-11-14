package yuanbao

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const installHint = "Yuanbao: websockets not installed. Run: pip install websockets"

// Setup menu row with the App ID and App Secret prompts
var setupPlatform = gateway.SetupPlatform{Key: "yuanbao", Label: "Yuanbao", Emoji: "💎", TokenVar: "YUANBAO_APP_ID",
	SetupInstructions: []string{
		"1. Download the Yuanbao app from https://yuanbao.tencent.com/",
		"2. In the app, go to PAI → My Bot and create a new bot",
		"3. After the bot is created, copy the App ID and App Secret",
		"4. Enter them below and Icebath will connect automatically over WebSocket",
	},
	Vars: []gateway.SetupVar{
		{Name: "YUANBAO_APP_ID", Prompt: "App ID", Help: "The App ID from your Yuanbao IM Bot credentials."},
		{Name: "YUANBAO_APP_SECRET", Prompt: "App Secret", Password: true,
			Help: "The App Secret (used for HMAC signing) from your Yuanbao IM Bot."},
	}}

// Whether the websocket client the adapter runs on is present
func checkYuanbaoRequirements() bool { return websocketsAvailable }

// Runs the standard App ID and App Secret prompts of the gateway setup
func interactiveSetup(ctx context.Context) error {
	return gateway.SetupStandardPlatform(ctx, setupPlatform)
}

// Whether the config extra or the env carries the App ID, or the legacy App Key, and the App Secret
func isConnected(ctx context.Context, config *gateway.PlatformConfig) (bool, error) {
	var extra map[string]any
	if config != nil {
		extra = config.Extra
	}
	appID := extra["app_id"]
	for _, name := range []string{"YUANBAO_APP_ID", "YUANBAO_APP_KEY"} {
		if pyrepr.Truthy(appID) {
			break
		}
		value, err := gateway.FacadeEnvValue(ctx, name)
		if err != nil {
			return false, err
		}
		appID = value
	}
	secret := extra["app_secret"]
	if !pyrepr.Truthy(secret) {
		value, err := gateway.FacadeEnvValue(ctx, "YUANBAO_APP_SECRET")
		if err != nil {
			return false, err
		}
		secret = value
	}
	return stripOrEmpty(appID) != "" && stripOrEmpty(secret) != "", nil
}

// Sends through the persistent signed connection of the running adapter, the only connection Yuanbao accepts sends on
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	mediaFiles, _ := kwargs["media_files"].([]gateway.MediaRef)
	adapter := GetActive(ctx)
	if adapter == nil {
		return gateway.SendError("Yuanbao adapter is not running. Start the gateway with yuanbao platform enabled first."), nil
	}
	return adapter.outbound.sender.sendDirect(ctx, chatID, message, mediaFiles)
}

// System prompt platform hint, cached prompt bytes kept stable
const platformHint = "You are on Yuanbao (腾讯元宝), a Chinese AI assistant " +
	"platform. Markdown renders (code blocks, tables, bold/italic). " +
	"You can send files natively: write MEDIA:/absolute/path/to/file in your response. " +
	"Images (.jpg, .png, .webp, .gif) send as photos, other files as downloadable documents (max 50 MB); " +
	"image URLs via ![alt](url) are downloaded and sent as photos. Never claim you lack file-sending. " +
	"Stickers (贴纸/表情包): when the user sends one (you see '[emoji: " +
	"名称]') or asks for one, use the sticker tools — yb_search_sticker with a Chinese " +
	"keyword, then yb_send_sticker with the chosen id — which send a real native sticker. Never " +
	"draw sticker-like PNGs and send them as images, and bare Unicode emoji is not a substitute."

// Group or direct prefixed reference with its surrounding whitespace
var prefixedSendTarget = gateway.RegexGroupsParser(`^\s*((?:group|direct):[^:]+)\s*$`, false)

// Target of a group or direct prefixed reference, a bare number as a group code and anything else not explicit
func parseSendTarget(ref string) any {
	if parsed := prefixedSendTarget(ref); parsed != nil {
		return parsed
	}
	if stripped := pystr.Strip(ref); pystr.StrIsDigit(stripped) {
		chatID := "group:" + stripped
		return &gateway.TargetRefParse{ChatID: &chatID}
	}
	return gateway.NotExplicit
}

// Builds the adapter for a platform config, the factory the registry and the adapter class share
func newAdapter(ctx context.Context, config *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
	adapter, err := NewYuanbaoAdapter(ctx, config)
	if err != nil {
		return nil, err
	}
	return adapter, nil
}

// Yuanbao platform plugin compiled into the binary under the bundled platforms/yuanbao key
type yuanbaoPlatformPlugin struct{}

func (yuanbaoPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "yuanbao-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/yuanbao",
		Author: "NousResearch",
		Description: "Tencent Yuanbao gateway adapter for Icebath Agent. Connects to Yuanbao IM Bot over a persistent WebSocket " +
			"(HMAC-signed sign token) and relays DMs and group messages between Yuanbao and the Icebath agent. Supports native " +
			"media/stickers, DM/group access policies, and home-channel cron delivery.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "YUANBAO_APP_ID", Description: "The App ID from your Yuanbao IM Bot credentials (legacy alias: YUANBAO_APP_KEY)"},
			{Name: "YUANBAO_APP_SECRET", Description: "The App Secret (used for HMAC signing) from your Yuanbao IM Bot", Secret: true}}}
}

// Registers the Yuanbao platform entry with its factory, checks, setup, target syntax, hint and standalone sender
func (yuanbaoPlatformPlugin) Register(ctx *plugins.Context) error {
	extra := map[string]any{
		"target_ref_syntax_fn":   parseSendTarget,
		"standalone_send":        &gateway.StandaloneSendPolicy{Label: "Yuanbao", Media: true, TextDirect: true},
		"dashboard_label":        "Yuanbao (元宝)",
		"dashboard_description":  "Connect Icebath to Tencent Yuanbao.",
		"dashboard_required_env": []string{},
		"is_connected":           isConnected,
		"config_connected_fn":    gateway.ConfigConnectedExtra("app_id", "app_secret"),
		"setup_fn":               interactiveSetup,
		"cron_deliver_env_var":   "YUANBAO_HOME_CHANNEL",
		"standalone_sender_fn":   standaloneSend,
		"emoji":                  "💎",
		"allow_update_command":   false,
		"platform_hint":          platformHint,
		"env_overrides":          envOverrides,
		"env_enable_credentials": envEnableCredentials,
	}
	for key, value := range authz {
		extra[key] = value
	}
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "yuanbao", Label: "Yuanbao",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, config *gateway.PlatformConfig) (any, error) {
			return newAdapter(fctx, config)
		}),
		Check:       checkYuanbaoRequirements,
		RequiredEnv: []string{"YUANBAO_APP_ID", "YUANBAO_APP_SECRET"},
		InstallHint: installHint,
		Extra:       extra,
	})
	return err
}

var _ gateway.PlatformAdapter = (*YuanbaoAdapter)(nil)

func init() {
	gateway.RegisterAdapterModule(&gateway.AdapterClass{Name: "YuanbaoAdapter", New: newAdapter, Traits: adapterTraits()})
	plugins.RegisterBundled(yuanbaoPlatformPlugin{})
}
