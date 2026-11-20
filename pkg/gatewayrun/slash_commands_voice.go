package gatewayrun

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Voice subcommand to the stored mode and its confirmation key
var voiceModeByArg = map[string][2]string{
	"on": {"voice_only", "gateway.voice.enabled_voice_only"}, "enable": {"voice_only", "gateway.voice.enabled_voice_only"},
	"off": {"off", "gateway.voice.disabled_text"}, "disable": {"off", "gateway.voice.disabled_text"},
	"tts": {"all", "gateway.voice.tts_enabled"},
}

// Discord adapter surface describing the voice channel the bot sits in for a guild
type voiceChannelInfoReader interface {
	GetVoiceChannelInfo(guildID int64) map[string]any
}

// Sets, reports or toggles the chat's voice reply mode and joins or leaves Discord voice channels
func (r *GatewayRunner) handleVoiceCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	args := pystr.Lower(pystr.Strip(event.GetCommandArgs()))
	chatID := event.Source.ChatID
	voiceKey := r.voiceKeyForSource(event.Source)
	adapter, err := r.deliveryAdapterFor(ctx, event.Source)
	if err != nil {
		return "", err
	}
	currentMode := func() string {
		if r.voiceMode == nil {
			return "off"
		}
		if mode, ok := r.voiceMode.Get(voiceKey); ok {
			return mode
		}
		return "off"
	}
	setMode := func(mode string) {
		r.setVoiceMode(voiceKey, mode)
		r.saveVoiceModes()
		gateway.LocalVoiceTransition(event.Source, mode)
		if adapter == nil {
			return
		}
		if mode == "off" {
			r.setAdapterAutoTTSDisabled(adapter, chatID, true)
		} else {
			r.setAdapterAutoTTSEnabled(adapter, chatID, true)
		}
	}
	if entry, ok := voiceModeByArg[args]; ok {
		setMode(entry[0])
		return i18n.T(ctx, entry[1], "", nil), nil
	}
	switch args {
	case "channel", "join":
		return r.handleVoiceChannelJoin(ctx, event)
	case "leave":
		return r.handleVoiceChannelLeave(ctx, event)
	case "status":
		mode := currentMode()
		label := mode
		if mode == "off" || mode == "voice_only" || mode == "all" {
			label = i18n.T(ctx, "gateway.voice.label_"+mode, "", nil)
		}
		lines := []string{i18n.T(ctx, "gateway.voice.status_mode", "", map[string]any{"label": label})}
		guildID, hasGuild, err := getGuildID(event)
		if err != nil {
			return "", err
		}
		var info map[string]any
		if reader, ok := adapter.(voiceChannelInfoReader); ok && hasGuild && guildID != 0 {
			info = reader.GetVoiceChannelInfo(guildID)
		}
		if len(info) > 0 {
			lines = append(lines, i18n.T(ctx, "gateway.voice.status_channel", "", map[string]any{"channel": info["channel_name"]}),
				i18n.T(ctx, "gateway.voice.status_participants", "", map[string]any{"count": info["member_count"]}))
			for _, member := range voiceMembers(info["members"]) {
				status := ""
				if pyrepr.Truthy(member["is_speaking"]) {
					status = i18n.T(ctx, "gateway.voice.speaking", "", nil)
				}
				lines = append(lines, i18n.T(ctx, "gateway.voice.status_member", "", map[string]any{"name": member["display_name"], "status": status}))
			}
		}
		return strings.Join(lines, "\n"), nil
	}
	turningOn := currentMode() == "off"
	toggleKey := "gateway.voice.disabled_short"
	if turningOn {
		setMode("voice_only")
		toggleKey = "gateway.voice.enabled_short"
	} else {
		setMode("off")
	}
	channels := ""
	if _, joins := adapter.(voiceChannelJoiner); adapter != nil && joins {
		channels = i18n.T(ctx, "gateway.voice.help_channels", "", nil)
	}
	return i18n.T(ctx, "gateway.voice.help", "", map[string]any{"toggle": i18n.T(ctx, toggleKey, "", nil), "channels": channels}), nil
}

// Member mappings of a voice channel info value
func voiceMembers(v any) []map[string]any {
	switch members := v.(type) {
	case []map[string]any:
		return members
	case []any:
		out := make([]map[string]any, 0, len(members))
		for _, m := range members {
			if mm, ok := m.(map[string]any); ok {
				out = append(out, mm)
			}
		}
		return out
	}
	return nil
}
