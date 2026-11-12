package telegram

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Plain text of a rich message inline node, its text or children flattened recursively
func flattenRichInlineText(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, item := range v {
			b.WriteString(flattenRichInlineText(item))
		}
		return b.String()
	case map[string]any:
		for _, key := range []string{"text", "children"} {
			if inner, ok := v[key]; ok && inner != nil {
				return flattenRichInlineText(inner)
			}
		}
	}
	return ""
}

// Plain text of rich message blocks, list items prefixed with their labels and trailing spaces trimmed
func flattenRichBlocks(blocks any) string {
	list, ok := blocks.([]any)
	if !ok {
		return ""
	}
	var lines []string
	for _, raw := range list {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if block["type"] == "list" {
			items, _ := block["items"].([]any)
			for _, rawItem := range items {
				item, ok := rawItem.(map[string]any)
				if !ok {
					continue
				}
				itemLines := pystr.Splitlines(flattenRichBlocks(item["blocks"]), false)
				if len(itemLines) == 0 {
					continue
				}
				if label := item["label"]; pyTruthy(label) {
					lines = append(lines, pystr.Strip(pyrepr.Str(label)+" "+itemLines[0]))
				} else {
					lines = append(lines, itemLines[0])
				}
				lines = append(lines, itemLines[1:]...)
			}
			continue
		}
		if text := flattenRichInlineText(block["text"]); text != "" {
			lines = append(lines, pystr.Splitlines(text, false)...)
		}
	}
	var kept []string
	for _, line := range lines {
		if line != "" {
			kept = append(kept, pystr.Rstrip(line))
		}
	}
	return strings.Join(kept, "\n")
}

// Plain text Telegram echoed in a reply's rich_message payload, nil when absent or blank
func extractRichReplyText(richMessage any) *string {
	rich, ok := richMessage.(map[string]any)
	if !ok {
		return nil
	}
	text := pystr.Strip(flattenRichBlocks(rich["blocks"]))
	if text == "" {
		return nil
	}
	return &text
}

// Notification mode all or important from the env, unknown values falling back to important
func resolveNotificationsMode(ctx context.Context) string {
	mode := os.Getenv("ICEBATH_TELEGRAM_NOTIFICATIONS")
	if mode == "" {
		if cfg, err := gateway.LoadGatewayConfig(ctx); err == nil {
			if raw, ok := config.CfgGet(config.AsMap(cfg), "display", "platforms", "telegram", "notifications"); ok && raw != nil && raw != "" {
				mode = pystr.Lower(pystr.Strip(pyrepr.Str(raw)))
			}
		}
	}
	if mode == "" {
		mode = "important"
	}
	if mode != "all" && mode != "important" {
		slog.Warn("Unknown telegram notifications mode '" + mode + "', defaulting to 'important' (valid: all, important)")
		mode = "important"
	}
	return mode
}

// Generic shared config keys the platform map merge already applied, never re-emitted from telegram extra
var genericMergeKeys = map[string]bool{"reply_prefix": true, "reply_in_thread": true, "reply_to_mode": true,
	"unauthorized_dm_behavior": true, "notice_delivery": true, "require_mention": true, "channel_skill_bindings": true,
	"channel_prompts": true, "gateway_restart_notification": true, "allow_from": true, "allow_admin_from": true, "dm_policy": true,
	"group_policy": true}

// Bridges config.yaml telegram keys into TELEGRAM env vars and returns the extras to merge, nil when none
func applyYAMLConfig(ctx context.Context, yamlCfg, telegramCfg map[string]any) (map[string]any, error) {
	extras := map[string]any{}
	setEnv := gateway.YAMLEnvSetter(ctx)
	setDefault := func(key string, value any) {
		if _, ok := extras[key]; !ok {
			extras[key] = value
		}
	}
	bridgeLower := func(key, env string) {
		if v, ok := telegramCfg[key]; ok {
			setDefault(key, v)
			setEnv(env, pystr.Lower(pyrepr.Str(v)))
		}
	}
	bridgeGate := func(key, env string, value any, seedExtra bool) {
		if value == nil {
			return
		}
		if seedExtra {
			setDefault(key, value)
		}
		setEnv(env, value)
	}
	for _, topicKey := range []string{"disable_topic_auto_rename", "dm_topic_auto_transfer"} {
		if v, ok := telegramCfg[topicKey]; ok {
			setDefault(topicKey, v)
		}
	}
	effectiveRM, ok := telegramCfg["require_mention"]
	if !ok {
		effectiveRM = yamlCfg["require_mention"]
	}
	if effectiveRM != nil {
		setEnv("TELEGRAM_REQUIRE_MENTION", pystr.Lower(pyrepr.Str(effectiveRM)))
	}
	if patterns, ok := telegramCfg["mention_patterns"]; ok {
		dumped, err := pyjson.Dumps(patterns)
		if err != nil {
			return nil, err
		}
		setEnv("TELEGRAM_MENTION_PATTERNS", dumped)
	}
	for _, pair := range [][2]string{{"exclusive_bot_mentions", "TELEGRAM_EXCLUSIVE_BOT_MENTIONS"}, {"allow_bots", "TELEGRAM_ALLOW_BOTS"},
		{"bots_require_mention", "TELEGRAM_BOTS_REQUIRE_MENTION"}, {"guest_mode", "TELEGRAM_GUEST_MODE"},
		{"observe_unmentioned_group_messages", "TELEGRAM_OBSERVE_UNMENTIONED_GROUP_MESSAGES"}} {
		bridgeLower(pair[0], pair[1])
	}
	for _, gate := range []struct {
		key, env string
		seed     bool
	}{{"free_response_chats", "TELEGRAM_FREE_RESPONSE_CHATS", true}, {"free_response_topics", "TELEGRAM_FREE_RESPONSE_TOPICS", false},
		{"allowed_chats", "TELEGRAM_ALLOWED_CHATS", false}, {"allowed_topics", "TELEGRAM_ALLOWED_TOPICS", false},
		{"ignored_threads", "TELEGRAM_IGNORED_THREADS", true}} {
		bridgeGate(gate.key, gate.env, telegramCfg[gate.key], gate.seed)
	}
	bridgeLower("reactions", "TELEGRAM_REACTIONS")
	if proxy, ok := telegramCfg["proxy_url"]; ok {
		setDefault("proxy_url", pystr.Strip(pyrepr.Str(proxy)))
		setEnv("TELEGRAM_PROXY", pystr.Strip(pyrepr.Str(proxy)))
	}
	telegramExtra, _ := telegramCfg["extra"].(map[string]any)
	if telegramExtra == nil {
		telegramExtra = map[string]any{}
	}
	rtm, hasRTM := telegramCfg["reply_to_mode"]
	if !hasRTM {
		rtm = telegramExtra["reply_to_mode"]
	}
	if rtm != nil {
		if b, isBool := rtm.(bool); isBool && !b {
			setEnv("TELEGRAM_REPLY_TO_MODE", "off")
		} else {
			setEnv("TELEGRAM_REPLY_TO_MODE", pystr.Lower(pyrepr.Str(rtm)))
		}
	}
	firstTruthy := func(values ...any) any {
		for _, v := range values[:len(values)-1] {
			if pyTruthy(v) {
				return v
			}
		}
		return values[len(values)-1]
	}
	bridgeGate("allow_from", "TELEGRAM_ALLOWED_USERS", telegramCfg["allow_from"], false)
	bridgeGate("group_allow_from", "TELEGRAM_GROUP_ALLOWED_USERS", firstTruthy(telegramCfg["group_allow_from"], telegramExtra["group_allow_from"]), false)
	bridgeGate("group_allowed_chats", "TELEGRAM_GROUP_ALLOWED_CHATS",
		firstTruthy(telegramCfg["group_allowed_chats"], telegramExtra["group_allowed_chats"]), false)
	for _, key := range []string{"guest_mode", "disable_link_previews", "observe_unmentioned_group_messages", "free_response_topics"} {
		if v, ok := telegramCfg[key]; ok {
			setDefault(key, v)
		}
	}
	for k, v := range telegramExtra {
		if !genericMergeKeys[k] {
			setDefault(k, v)
		}
	}
	if len(extras) == 0 {
		return nil, nil
	}
	return extras, nil
}
