package discord

import (
	"strings"

	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Voice effect settings of discord.voice_fx, read once when the adapter is built
type voiceFXConfig struct {
	Enabled        bool
	AmbientEnabled bool
	AmbientPath    string
	AmbientGain    float64
	DuckGain       float64
	SpeechGain     float64
	LeadSilence    time.Duration
	AckEnabled     bool
	AckPhrases     []string
}

// Voice effect settings of the profile ctx serves
func loadVoiceFXConfig(ctx context.Context) voiceFXConfig {
	sctx := gateway.SettingsContext(ctx)
	cfg := voiceFXConfig{
		Enabled:        settings.DiscordVoiceFxEnabled.Get(sctx),
		AmbientEnabled: settings.DiscordVoiceFxAmbientEnabled.Get(sctx),
		AmbientPath:    strings.TrimSpace(settings.DiscordVoiceFxAmbientPath.Get(sctx)),
		AmbientGain:    settings.DiscordVoiceFxAmbientGain.Get(sctx),
		DuckGain:       settings.DiscordVoiceFxDuckGain.Get(sctx),
		SpeechGain:     settings.DiscordVoiceFxSpeechGain.Get(sctx),
		LeadSilence:    settings.DiscordVoiceFxLeadSilenceMs.Get(sctx),
		AckEnabled:     settings.DiscordVoiceFxAckEnabled.Get(sctx),
	}
	for _, phrase := range settings.DiscordVoiceFxAckPhrases.Strings(sctx) {
		if phrase = strings.TrimSpace(phrase); phrase != "" {
			cfg.AckPhrases = append(cfg.AckPhrases, phrase)
		}
	}
	return cfg
}
