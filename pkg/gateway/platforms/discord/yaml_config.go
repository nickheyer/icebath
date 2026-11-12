package discord

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var yamlBoolEnvKeys = [][2]string{
	{"require_mention", "DISCORD_REQUIRE_MENTION"},
	{"thread_require_mention", "DISCORD_THREAD_REQUIRE_MENTION"},
	{"bots_require_inline_mention", "DISCORD_BOTS_REQUIRE_INLINE_MENTION"},
}

// Public websocket key, legacy liveness alias and env bridge variable of each liveness knob
var yamlWebsocketLivenessKeys = [][3]string{
	{"websocket_liveness_interval_seconds", "liveness_interval_seconds", "ICEBATH_DISCORD_LIVENESS_INTERVAL_SECONDS"},
	{"websocket_liveness_failure_threshold", "liveness_failure_threshold", "ICEBATH_DISCORD_LIVENESS_FAILURE_THRESHOLD"},
	{"websocket_heartbeat_ack_max_age_seconds", "", ""},
	{"websocket_max_latency_seconds", "", ""},
	{"websocket_event_max_silence_seconds", "", ""},
}

// Python str lowered
func lowerStr(v any) string { return strings.ToLower(pyrepr.Str(v)) }

// Comma joined list items, else the value's text
func csvText(v any) string {
	if list, ok := v.([]any); ok {
		parts := make([]string, len(list))
		for i, x := range list {
			parts[i] = pyrepr.Str(x)
		}
		return strings.Join(parts, ",")
	}
	return pyrepr.Str(v)
}

// Seeds the discord config keys into extra and bridges them to first writer wins env vars
func applyYAMLConfig(ctx context.Context, yamlCfg, discordCfg map[string]any) (map[string]any, error) {
	envDefault := gateway.YAMLEnvSetter(ctx)
	seeded := map[string]any{}
	for _, kv := range yamlBoolEnvKeys {
		if v, ok := discordCfg[kv[0]]; ok {
			seeded[kv[0]] = v
			envDefault(kv[1], lowerStr(v))
		}
	}
	platformExtra := map[string]any{}
	if platforms, ok := yamlCfg["platforms"].(map[string]any); ok {
		if dp, ok := platforms["discord"].(map[string]any); ok {
			if extra, ok := dp["extra"].(map[string]any); ok {
				platformExtra = extra
			}
		}
	}
	gate := func(key, envKey string, fromPlatformExtra, lower bool) {
		value, ok := discordCfg[key]
		if !ok {
			value = nil
			if fromPlatformExtra {
				value = platformExtra[key]
			}
		}
		if value == nil {
			return
		}
		text := csvText(value)
		if lower {
			text = lowerStr(value)
		}
		seeded[key] = text
		envDefault(envKey, text)
	}
	gate("allow_from", "DISCORD_ALLOWED_USERS", true, false)
	gate("allowed_roles", "DISCORD_ALLOWED_ROLES", true, false)
	gate("allow_all_users", "DISCORD_ALLOW_ALL_USERS", true, true)
	gate("allow_bots", "DISCORD_ALLOW_BOTS", true, true)
	approval, ok := discordCfg["approval_mentions"]
	if !ok {
		approval = platformExtra["approval_mentions"]
	}
	if approval != nil {
		seeded["approval_mentions"] = approval
		envDefault("DISCORD_APPROVAL_MENTIONS", lowerStr(approval))
	}
	gate("free_response_channels", "DISCORD_FREE_RESPONSE_CHANNELS", false, false)
	for _, kv := range [][2]string{{"auto_thread", "DISCORD_AUTO_THREAD"}, {"free_response_auto_thread", "DISCORD_FREE_RESPONSE_AUTO_THREAD"}, {"reactions", "DISCORD_REACTIONS"}} {
		if v, ok := discordCfg[kv[0]]; ok {
			seeded[kv[0]] = v
			envDefault(kv[1], lowerStr(v))
		}
	}
	if backfill, ok := discordCfg["missed_message_backfill"].(map[string]any); ok {
		cp := make(map[string]any, len(backfill))
		for k, v := range backfill {
			cp[k] = v
		}
		seeded["missed_message_backfill"] = cp
	}
	gate("ignored_channels", "DISCORD_IGNORED_CHANNELS", false, false)
	gate("allowed_channels", "DISCORD_ALLOWED_CHANNELS", false, false)
	gate("no_thread_channels", "DISCORD_NO_THREAD_CHANNELS", false, false)
	if v, ok := discordCfg["history_backfill"]; ok {
		seeded["history_backfill"] = v
		envDefault("DISCORD_HISTORY_BACKFILL", lowerStr(v))
	}
	if hbl := discordCfg["history_backfill_limit"]; hbl != nil {
		seeded["history_backfill_limit"] = hbl
		envDefault("DISCORD_HISTORY_BACKFILL_LIMIT", pyrepr.Str(hbl))
	}
	if mentions, ok := discordCfg["allow_mentions"].(map[string]any); ok {
		cp := make(map[string]any, len(mentions))
		for k, v := range mentions {
			cp[k] = v
		}
		seeded["allow_mentions"] = cp
		for _, key := range []string{"everyone", "roles", "users", "replied_user"} {
			if v, has := mentions[key]; has {
				envDefault("DISCORD_ALLOW_MENTION_"+strings.ToUpper(key), lowerStr(v))
			}
		}
	}
	discordExtra, _ := discordCfg["extra"].(map[string]any)
	if discordExtra == nil {
		discordExtra = map[string]any{}
	}
	rtm, ok := discordCfg["reply_to_mode"]
	if !ok {
		rtm = discordExtra["reply_to_mode"]
	}
	if rtm != nil {
		if rtm == false {
			envDefault("DISCORD_REPLY_TO_MODE", "off")
		} else {
			envDefault("DISCORD_REPLY_TO_MODE", lowerStr(rtm))
		}
	}
	liveness := map[string]any{}
	for k, v := range discordExtra {
		liveness[k] = v
	}
	for k, v := range discordCfg {
		liveness[k] = v
	}
	for _, row := range yamlWebsocketLivenessKeys {
		value := liveness[row[0]]
		if value == nil && row[1] != "" {
			value = liveness[row[1]]
		}
		if value != nil {
			seeded[row[0]] = value
			if row[2] != "" {
				envDefault(row[2], pyrepr.Str(value))
			}
		}
	}
	if len(seeded) == 0 {
		return nil, nil
	}
	return seeded, nil
}
