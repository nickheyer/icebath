package feishu

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

var migrationImageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true}
var migrationVideoExts = map[string]bool{".mp4": true, ".mov": true, ".avi": true, ".mkv": true, ".webm": true, ".3gp": true}
var migrationAudioExts = map[string]bool{".ogg": true, ".opus": true, ".mp3": true, ".wav": true, ".m4a": true, ".flac": true}

// Feishu chat, user and thread target forms of send_message
const sendTargetPattern = `^\s*((?:oc|ou|on|chat|open)_[-A-Za-z0-9]+)(?::([-A-Za-z0-9_]+))?\s*$`

var parseSendTarget = gateway.RegexGroupsParser(sendTargetPattern, true)

// Out of process delivery through a transient adapter, text first then each media file by its kind
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	threadID, _ := kwargs["thread_id"].(string)
	mediaFiles, _ := kwargs["media_files"].([]gateway.MediaRef)
	adapter, err := NewFeishuAdapter(ctx, pconfig)
	if err != nil {
		return gateway.SendError("Feishu send failed: " + err.Error()), nil
	}
	adapter.mu.Lock()
	adapter.client = adapter.buildLarkClient(sdkDomain(adapter.settings.DomainName))
	adapter.mu.Unlock()
	var metadata map[string]any
	if threadID != "" {
		metadata = map[string]any{"thread_id": threadID}
	}
	var last *gateway.SendResult
	if pystr.Strip(message) != "" {
		r, err := adapter.Send(ctx, chatID, message, "", metadata)
		if err != nil {
			return gateway.SendError("Feishu send failed: " + err.Error()), nil
		}
		last = &r
		if !r.Success {
			return gateway.SendError("Feishu send failed: " + pyrepr.Str(r.Error)), nil
		}
	}
	for _, media := range mediaFiles {
		if _, err := os.Stat(media.Path); err != nil {
			return gateway.SendError("Media file not found: " + media.Path), nil
		}
		ext := strings.ToLower(filepath.Ext(media.Path))
		var r gateway.SendResult
		switch {
		case migrationImageExts[ext]:
			r, err = adapter.SendImageFile(ctx, chatID, media.Path, "", "", metadata, nil)
		case migrationVideoExts[ext]:
			r, err = adapter.SendVideo(ctx, chatID, media.Path, "", "", metadata, nil)
		case migrationAudioExts[ext]:
			r, err = adapter.SendVoice(ctx, chatID, media.Path, "", "", metadata, nil)
		default:
			r, err = adapter.SendDocument(ctx, chatID, media.Path, "", "", "", metadata, nil)
		}
		if err != nil {
			return gateway.SendError("Feishu send failed: " + err.Error()), nil
		}
		last = &r
		if !r.Success {
			return gateway.SendError("Feishu media send failed: " + pyrepr.Str(r.Error)), nil
		}
	}
	if last == nil {
		return gateway.SendError("No deliverable text or media remained after processing MEDIA tags"), nil
	}
	return map[string]any{"success": true, "platform": "feishu", "chat_id": chatID, "message_id": last.MessageID}, nil
}

// Bridges feishu.allow_bots into FEISHU_ALLOW_BOTS, env winning, and seeds the extra a secondary profile reads
func applyYAMLConfig(ctx context.Context, _ map[string]any, feishuCfg map[string]any) (map[string]any, error) {
	seeded, err := gateway.ApplyYAMLBridge(ctx, feishuCfg, []gateway.YAMLBridgeRow{{Key: "allow_bots", Env: "FEISHU_ALLOW_BOTS", Kind: "lower"}})
	if err != nil || len(seeded) == 0 {
		return nil, err
	}
	return map[string]any{"allow_bots": pystr.Lower(pyrepr.Str(seeded["allow_bots"]))}, nil
}

// Connected once an app id is configured
func isConnected(cfg *gateway.PlatformConfig) bool {
	return cfg != nil && cfg.Extra != nil && pyrepr.Truthy(cfg.Extra["app_id"])
}

// Passive dependency check, satisfied since the lark SDK is compiled into the binary
func feishuDepsPresent() bool { return true }

// Active dependency check, satisfied since the lark SDK is compiled into the binary
func checkFeishuRequirements(context.Context) (bool, error) { return true, nil }

// Feishu platform plugin compiled into the binary under the bundled platforms/feishu key
type feishuPlatformPlugin struct{}

func (feishuPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "feishu-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/feishu",
		Author: "NousResearch",
		Description: "Feishu / Lark gateway adapter for Icebath Agent. Connects to Feishu (China) or Lark (International) via the official " +
			"lark SDK over WebSocket or webhook and relays messages between Feishu/Lark chats and the Icebath agent. Supports text, images, " +
			"video, voice, documents, threads, DM pairing, group @mention gating, drive comment events, and meeting invites.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "FEISHU_APP_ID", Description: "Feishu/Lark app ID", URL: "https://open.feishu.cn/"},
			{Name: "FEISHU_APP_SECRET", Description: "Feishu/Lark app secret", URL: "https://open.feishu.cn/", Secret: true}}}
}

// Registers the Feishu platform entry with its factory, setup, YAML bridge, standalone sender and card
func (feishuPlatformPlugin) Register(ctx *plugins.Context) error {
	var card gateway.PlatformEntry
	applyDashboardCard(&card)
	extra := map[string]any{
		"target_ref_syntax_fn":  parseSendTarget,
		"standalone_send":       &gateway.StandaloneSendPolicy{Label: "Feishu", Media: true, TextDirect: true},
		"dashboard_description": card.DashboardDescription, "docs_url": card.DocsURL,
		"dashboard_env_vars": card.DashboardEnvVars, "env_value_rules": card.EnvValueRules,
		"ensure_deps_fn":         checkFeishuRequirements,
		"is_connected":           isConnected,
		"validate_config":        isConnected,
		"setup_fn":               InteractiveSetup,
		"apply_yaml_config_fn":   applyYAMLConfig,
		"cron_deliver_env_var":   "FEISHU_HOME_CHANNEL",
		"standalone_sender_fn":   standaloneSend,
		"max_message_length":     maxMessageLength,
		"emoji":                  "🪽",
		"allow_update_command":   true,
		"platform_hint":          platformHint,
		"display_tier":           "medium",
		"env_overrides":          envOverrides,
		"env_enable_credentials": envEnableCredentials,
	}
	for k, v := range authz {
		extra[k] = v
	}
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "feishu", Label: "Feishu / Lark",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return NewFeishuAdapter(fctx, cfg)
		}),
		Check:       feishuDepsPresent,
		RequiredEnv: []string{"FEISHU_APP_ID", "FEISHU_APP_SECRET"},
		InstallHint: "Run `icebath setup` to install Feishu support.",
		Extra:       extra,
	})
	return err
}

// Publishes the bundled plugin and the adapter class the plugin factory's module holds
func init() {
	plugins.RegisterBundled(feishuPlatformPlugin{})
	gateway.RegisterAdapterModule(&gateway.AdapterClass{Name: "FeishuAdapter", Traits: feishuAdapterTraits(),
		New: func(ctx context.Context, cfg *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
			return NewFeishuAdapter(ctx, cfg)
		}})
}
