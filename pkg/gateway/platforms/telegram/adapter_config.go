package telegram

import (
	"context"
	"slices"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/settings"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
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
				itemLines := textutil.Lines(flattenRichBlocks(item["blocks"]))
				if len(itemLines) == 0 {
					continue
				}
				if label := item["label"]; pyTruthy(label) {
					lines = append(lines, strings.TrimSpace(pyrepr.Str(label)+" "+itemLines[0]))
				} else {
					lines = append(lines, itemLines[0])
				}
				lines = append(lines, itemLines[1:]...)
			}
			continue
		}
		if text := flattenRichInlineText(block["text"]); text != "" {
			lines = append(lines, textutil.Lines(text)...)
		}
	}
	var kept []string
	for _, line := range lines {
		if line != "" {
			kept = append(kept, strings.TrimRightFunc(line, unicode.IsSpace))
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
	text := strings.TrimSpace(flattenRichBlocks(rich["blocks"]))
	if text == "" {
		return nil
	}
	return &text
}

// Notification mode of the adapter's profile as set right now, all or important
func (a *TelegramAdapter) notificationsMode() string {
	return settings.TelegramNotifications.Get(a.settingsCtx)
}

// Generic shared config keys the platform map merge already applied, never re-emitted from telegram extra
var genericMergeKeys = map[string]bool{"reply_prefix": true, "reply_in_thread": true, "reply_to_mode": true,
	"unauthorized_dm_behavior": true, "notice_delivery": true, "require_mention": true, "channel_skill_bindings": true,
	"channel_prompts": true, "gateway_restart_notification": true, "allow_from": true, "allow_admin_from": true, "dm_policy": true,
	"group_policy": true}

// Keys of the telegram section still handed to the adapter through its platform extra; the rest are settings
var extraPassthroughKeys = []string{"disable_topic_auto_rename", "dm_topic_auto_transfer", "disable_link_previews"}

// Extras of the config.yaml telegram section the adapter reads from its platform config, nil when none
func applyYAMLConfig(ctx context.Context, yamlCfg, telegramCfg map[string]any) (map[string]any, error) {
	extras := map[string]any{}
	for _, key := range extraPassthroughKeys {
		if v, ok := telegramCfg[key]; ok {
			extras[key] = v
		}
	}
	if proxy, ok := telegramCfg["proxy_url"]; ok {
		extras["proxy_url"] = strings.TrimSpace(pyrepr.Str(proxy))
	}
	telegramExtra, _ := telegramCfg["extra"].(map[string]any)
	for k, v := range telegramExtra {
		if _, set := extras[k]; set || genericMergeKeys[k] {
			continue
		}
		if slices.Contains(extraPassthroughKeys, k) || (settings.Find("telegram."+k) == nil && settings.Find("telegram.extra."+k) == nil) {
			extras[k] = v
		}
	}
	if len(extras) == 0 {
		return nil, nil
	}
	return extras, nil
}
