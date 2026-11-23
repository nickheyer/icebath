package discord

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Defaults of discord.voice_fx overlaid with the non null keys config.yaml sets
func loadVoiceFXConfig(ctx context.Context) map[string]any {
	defaults := map[string]any{"enabled": false, "ambient_enabled": true, "ambient_path": "", "ambient_gain": 0.18, "duck_gain": 0.06,
		"speech_gain": 1.0, "lead_silence_ms": 200, "ack_enabled": true, "ack_phrases": nil}
	cfg, err := config.ReadRawConfig(ctx, home.Dir(ctx))
	if err != nil {
		pkgLog().Debug("Could not load discord.voice_fx config: " + err.Error())
		return defaults
	}
	section, _ := cfg["discord"].(map[string]any)
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
