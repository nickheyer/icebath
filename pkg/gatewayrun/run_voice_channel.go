package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Discord voice channel a member sits in
type voiceChannelRef = interface{ Name() string }

// Discord adapter surface joining voice channels and binding them to the requesting text channel
type voiceChannelJoiner interface {
	voiceInputBinder
	voiceTextChannelBinder
	GetUserVoiceChannel(ctx context.Context, guildID int64, userID *string) (voiceChannelRef, error)
	JoinVoiceChannel(ctx context.Context, channel voiceChannelRef) (bool, error)
	BindVoiceTextChannel(guildID, textChannelID int64)
}

// Discord adapter hook told when its voice connection times out
type voiceDisconnectNotifier interface {
	SetOnVoiceDisconnect(fn func(chatID string))
}

// Discord adapter hook reading the live voice reply mode of a text channel
type voiceModeGetterHolder interface {
	SetVoiceModeGetter(fn func(chatID any) string)
}

// Discord adapter surface dropping speech buffered for a guild's previous text channel
type pendingVoiceDiscarder interface {
	DiscardPendingVoiceInput(guildID int64)
}

// Discord adapter surface remembering the source a voice join came from
type voiceSourceRecorder interface {
	SetVoiceSource(guildID int64, source *pyjson.Object)
}

// Discord adapter surface leaving a guild's voice channel
type voiceChannelLeaver interface {
	LeaveVoiceChannel(ctx context.Context, guildID int64) error
	IsInVoiceChannel(guildID int64) bool
}

// Joins the caller's Discord voice channel and binds its speech to this text channel
func (r *GatewayRunner) handleVoiceChannelJoin(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	adapter, err := r.deliveryAdapterFor(ctx, event.Source)
	if err != nil {
		return "", err
	}
	joiner, ok := adapter.(voiceChannelJoiner)
	if !ok {
		return i18n.T(ctx, "gateway.voice.channel_unsupported", "", nil), nil
	}
	guildID, hasGuild, err := getGuildID(event)
	if err != nil {
		return "", err
	}
	if !hasGuild || guildID == 0 {
		return i18n.T(ctx, "gateway.voice.channel_guild_only", "", nil), nil
	}
	voiceChannel, err := joiner.GetUserVoiceChannel(ctx, guildID, event.Source.UserID)
	if err != nil {
		return "", err
	}
	if voiceChannel == nil {
		return i18n.T(ctx, "gateway.voice.channel_not_in_voice", "", nil), nil
	}
	r.bindVoiceInputCallback(ctx, adapter)
	voiceProfile := r.adapterProfileForSource(event.Source)
	if notifier, ok := adapter.(voiceDisconnectNotifier); ok {
		notifier.SetOnVoiceDisconnect(func(chatID string) { r.handleVoiceTimeoutCleanup(chatID, adapter) })
	}
	if holder, ok := adapter.(voiceModeGetterHolder); ok {
		holder.SetVoiceModeGetter(func(chatID any) string {
			if r.voiceMode == nil {
				return "off"
			}
			mode, found := r.voiceMode.Get(voiceKey(gateway.PlatformDiscord, pyrepr.Str(chatID), voiceProfile))
			if !found {
				return "off"
			}
			return mode
		})
	}
	success, err := joiner.JoinVoiceChannel(ctx, voiceChannel)
	if err != nil {
		runLog().Warn(fmt.Sprintf("Failed to join voice channel: %s", pyrepr.ExceptionMessage(err)))
		joiner.SetVoiceInputCallback(nil)
		var native interface{ VoiceDependencyHint() string }
		if errors.As(err, &native) {
			return i18n.T(ctx, "gateway.voice.channel_deps_missing", "", map[string]any{"hint": native.VoiceDependencyHint()}), nil
		}
		return i18n.T(ctx, "gateway.voice.channel_join_failed", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}), nil
	}
	if !success {
		joiner.SetVoiceInputCallback(nil)
		return i18n.T(ctx, "gateway.voice.channel_join_permissions", "", nil), nil
	}
	textChannelID, err := strconv.ParseInt(pystr.Strip(event.Source.ChatID), 10, 64)
	if err != nil {
		return "", fmt.Errorf("ValueError: invalid literal for int() with base 10: %s", pyrepr.Repr(event.Source.ChatID))
	}
	if previous, found := joiner.VoiceTextChannel(guildID); found && previous != textChannelID {
		if discarder, ok := adapter.(pendingVoiceDiscarder); ok {
			discarder.DiscardPendingVoiceInput(guildID)
		}
	}
	joiner.BindVoiceTextChannel(guildID, textChannelID)
	if recorder, ok := adapter.(voiceSourceRecorder); ok {
		recorder.SetVoiceSource(guildID, event.Source.ToDict())
	}
	r.applyVoiceMode(adapter, r.voiceKeyForSource(event.Source), event.Source.ChatID, "all")
	return i18n.T(ctx, "gateway.voice.channel_joined", "", map[string]any{"name": voiceChannel.Name()}), nil
}

// Leaves the guild's voice channel and turns this chat's voice replies off even when leaving failed
func (r *GatewayRunner) handleVoiceChannelLeave(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	adapter, err := r.deliveryAdapterFor(ctx, event.Source)
	if err != nil {
		return "", err
	}
	guildID, hasGuild, err := getGuildID(event)
	if err != nil {
		return "", err
	}
	leaver, ok := adapter.(voiceChannelLeaver)
	if !hasGuild || guildID == 0 || !ok || !leaver.IsInVoiceChannel(guildID) {
		return i18n.T(ctx, "gateway.voice.channel_not_joined", "", nil), nil
	}
	if err := leaver.LeaveVoiceChannel(ctx, guildID); err != nil {
		runLog().Warn(fmt.Sprintf("Error leaving voice channel: %s", pyrepr.ExceptionMessage(err)))
	}
	r.applyVoiceMode(adapter, r.voiceKeyForSource(event.Source), event.Source.ChatID, "off")
	if binder, ok := adapter.(voiceInputBinder); ok {
		binder.SetVoiceInputCallback(nil)
	}
	return i18n.T(ctx, "gateway.voice.channel_left", "", nil), nil
}

// Clears the voice mode of a timed out voice channel on the bot whose call it was
func (r *GatewayRunner) handleVoiceTimeoutCleanup(chatID string, adapter gateway.PlatformAdapter) {
	if adapter == nil && r.adapters != nil {
		adapter, _ = r.adapters.Get(gateway.PlatformDiscord)
	}
	var profile *string
	if adapter != nil && adapter.Base() != nil {
		if owner := adapter.Base().OwnerProfile(); owner != "" {
			profile = &owner
		}
	}
	r.applyVoiceMode(adapter, voiceKey(gateway.PlatformDiscord, chatID, profile), chatID, "off")
}
