package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Voice reply modes a chat may hold
var voiceModeValues = map[string]bool{"off": true, "voice_only": true, "all": true}

// Context of the per chat voice modes, which live in the process home's settings
func voiceModeCtx() context.Context {
	return home.WithOverride(context.Background(), home.Process())
}

// Voice reply mode a chat chose, false when it chose none
func chatVoiceMode(key string) (string, bool) {
	mode, _ := settings.ChatVoiceModes.Get(voiceModeCtx())[key].(string)
	return mode, voiceModeValues[mode]
}

// Every chat's chosen voice reply mode, keyed by chat
func chatVoiceModes() map[string]string {
	out := map[string]string{}
	for key, raw := range settings.ChatVoiceModes.Get(voiceModeCtx()) {
		if mode, _ := raw.(string); voiceModeValues[mode] {
			out[key] = mode
		}
	}
	return out
}

// Stores a chat's voice reply mode, a failed write logged
func storeChatVoiceMode(key, mode string) {
	if _, err := settings.ChatVoiceModes.SetEntry(voiceModeCtx(), key, mode); err != nil {
		pkgLog().Warn(fmt.Sprintf("Failed to save the voice mode of %s: %s", key, errchain.Show(err)))
	}
}
