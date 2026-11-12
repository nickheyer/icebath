package discord

import (
	"context"
	"fmt"
	"log/slog"
)

const (
	voiceTimeoutDefault    = 300
	playbackTimeoutDefault = 120
)

// Defaults of discord.voice_fx overlaid with the non null keys config.yaml sets
func loadVoiceFXConfig(ctx context.Context) map[string]any {
	defaults := map[string]any{"enabled": false, "ambient_enabled": true, "ambient_path": "", "ambient_gain": 0.18, "duck_gain": 0.06,
		"speech_gain": 1.0, "lead_silence_ms": 200, "ack_enabled": true, "ack_phrases": nil}
	section, ok := rawSection(ctx, "discord")
	if !ok {
		slog.Debug("Could not load discord.voice_fx config: 'discord' is not a mapping")
		return defaults
	}
	fx, isMap := section["voice_fx"].(map[string]any)
	if !isMap {
		return defaults
	}
	for k, v := range fx {
		if _, known := defaults[k]; known && v != nil {
			defaults[k] = v
		}
	}
	return defaults
}

// Non secret integer of the top level discord config, floored at minimum, the default when unreadable
func loadDiscordIntConfig(ctx context.Context, key string, def, minimum int) int {
	section, ok := rawSection(ctx, "discord")
	if !ok {
		slog.Debug(fmt.Sprintf("Could not load discord.%s config: 'discord' is not a mapping", key))
		return def
	}
	raw, present := section[key]
	if !present {
		raw = def
	}
	n, ok, err := configInt(raw)
	if err != nil || !ok || !n.IsInt64() {
		slog.Debug(fmt.Sprintf("Could not load discord.%s config: %v", key, raw))
		return def
	}
	return max(minimum, int(n.Int64()))
}

// Voice channel inactivity timeout seconds, zero disabling it
func loadVoiceTimeout(ctx context.Context) int {
	return loadDiscordIntConfig(ctx, "voice_channel_inactivity_timeout_seconds", voiceTimeoutDefault, 0)
}

// Minimum playback wait seconds for voice channel audio
func loadPlaybackTimeout(ctx context.Context) int {
	return loadDiscordIntConfig(ctx, "voice_playback_timeout_seconds", playbackTimeoutDefault, 1)
}
